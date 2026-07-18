// Package main — actuator + output-writer edge for claude-steward.
//
// # Purpose
//
// This file turns the pure policy core's steward.Action values into real
// effects: driving a tmux pane or a background worker's PTY socket to send
// "/compact", publishing human-facing notices on nervous-bus, and atomically
// persisting the public budget.json / gate.json contracts. Everything here
// performs I/O; internal/steward never does.
//
// # Design constraints
//
//   - osActuator's shell-out and unix-dial calls are injectable function
//     fields (runCmd, dialUnix) so tests exercise the real encode/handshake/
//     safety logic against a fake process and an in-memory net.Pipe conn,
//     never a real tmux server or a real daemon socket.
//   - The MethodTmux send-keys protocol is a one-shot, defense-in-depth
//     safety gate: re-check the pause file, verify the pane is actually
//     running `claude` (never inject into a bare shell or editor), audit
//     BEFORE sending, send literal text then Enter as two separate calls,
//     no retry loop.
//   - The MethodPtySock protocol is a small framed binary handshake against
//     the daemon's bg-pty-host worker: 5-byte header (uint32 BE payload
//     length + 1 byte kind), kind 1 = control JSON, kind 0 = raw PTY bytes.
//     The ptyAuth secret is read locally from the roster for the handshake
//     only and is never logged or written to the audit trail.
//   - budget.json / gate.json are written atomically (temp file + rename in
//     the same directory) so a concurrent reader never observes a partial
//     write.
//
// # Failure modes
//
//   - Pause file present → Compact aborts before any send, on both methods.
//   - Pane not running claude → abort, no keys sent.
//   - Any runCmd / dial / handshake error is returned verbatim so the daemon
//     can mark the session do-not-touch; there is no internal retry.
//   - Notify is fire-and-forget: bus publish errors are swallowed, never
//     returned.
//   - Audit-log and roster-read failures never panic; audit swallows its own
//     errors (best-effort trail), roster/handshake failures propagate as the
//     Compact error.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

// Compile-time interface conformance checks.
var (
	_ steward.Actuator     = (*osActuator)(nil)
	_ steward.Notifier     = (*busNotifier)(nil)
	_ steward.BudgetWriter = (*fileBudgetWriter)(nil)
	_ steward.GateWriter   = (*fileGateWriter)(nil)
)

// ---------------------------------------------------------------------------
// osActuator — steward.Actuator
// ---------------------------------------------------------------------------

// osActuator drives real sessions via tmux send-keys or the bg-pty-host
// framed socket. The runCmd and dialUnix fields are injectable so unit tests
// never shell out or dial a real socket; newOSActuator wires the real
// implementations.
type osActuator struct {
	runCmd     func(ctx context.Context, name string, args ...string) (string, error)
	dialUnix   func(ctx context.Context, path string) (net.Conn, error)
	auditPath  string // ~/.claude/steward/actions.jsonl
	pausePath  string // ~/.claude/steward/pause
	rosterPath string // ~/.claude/daemon/roster.json
}

// newOSActuator builds an osActuator wired to the real world: os/exec for
// tmux, a real unix-socket dial, and the default ~/.claude paths.
func newOSActuator() *osActuator {
	dir := defaultStewardDir()
	return &osActuator{
		runCmd:     realRunCmd,
		dialUnix:   realDialUnix,
		auditPath:  filepath.Join(dir, "actions.jsonl"),
		pausePath:  filepath.Join(dir, "pause"),
		rosterPath: defaultRosterPath(),
	}
}

// realRunCmd is the production runCmd: a bounded (5s) exec.CommandContext
// whose combined stdout is returned trimmed of nothing (callers trim as
// needed) — never a bare exec.Command with no deadline.
func realRunCmd(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).Output()
	return string(out), err
}

// realDialUnix is the production dialUnix: a context-bounded unix-socket
// dial against the bg-pty-host worker.
func realDialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// Compact routes to the tmux or ptysock protocol per the pre-resolved
// method, re-checking the pause file first (defense in depth — the daemon
// also checks before calling, but a wrong send here is dangerous enough to
// warrant a second gate).
func (a *osActuator) Compact(ctx context.Context, s steward.SessionState, method steward.Method, reason string) error {
	if a.isPaused() {
		return errors.New("actuators: steward paused, refusing to compact")
	}
	switch method {
	case steward.MethodTmux:
		return a.compactTmux(ctx, s, reason)
	case steward.MethodPtySock:
		return a.compactPtySock(ctx, s, reason)
	default:
		return fmt.Errorf("actuators: session %s has no actuable method (%s)", s.ID, method)
	}
}

func (a *osActuator) isPaused() bool {
	if a.pausePath == "" {
		return false
	}
	_, err := os.Stat(a.pausePath)
	return err == nil
}

// --- MethodTmux ---

// parseTmuxPane extracts the tmux pane target ("%12") from a SessionState
// PaneID of the form "tmux:%12". Any other form (empty, "zellij:...") is
// rejected — this actuator only drives tmux for now.
func parseTmuxPane(paneID string) (string, error) {
	const prefix = "tmux:"
	if !strings.HasPrefix(paneID, prefix) {
		return "", fmt.Errorf("actuators: not a tmux pane: %q", paneID)
	}
	pane := strings.TrimPrefix(paneID, prefix)
	if pane == "" {
		return "", fmt.Errorf("actuators: empty tmux pane in %q", paneID)
	}
	return pane, nil
}

// compactTmux implements the send-keys safety protocol exactly:
// verify the pane is running claude, audit before sending, send the literal
// "/compact" then a separate Enter, no retry.
func (a *osActuator) compactTmux(ctx context.Context, s steward.SessionState, reason string) error {
	pane, err := parseTmuxPane(s.PaneID)
	if err != nil {
		return err
	}

	out, err := a.runCmd(ctx, "tmux", "display", "-p", "-t", pane, "#{pane_current_command}")
	if err != nil {
		return fmt.Errorf("actuators: tmux display failed for %s: %w", pane, err)
	}
	if !strings.Contains(strings.TrimSpace(out), "claude") {
		return fmt.Errorf("actuators: pane %s is not running claude (got %q), refusing to send keys", pane, strings.TrimSpace(out))
	}

	a.audit(map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339),
		"action":     "compact",
		"session_id": s.ID,
		"method":     steward.MethodTmux.String(),
		"reason":     reason,
		"sent":       false,
	})

	if _, err := a.runCmd(ctx, "tmux", "send-keys", "-t", pane, "-l", "/compact"); err != nil {
		return fmt.Errorf("actuators: tmux send-keys literal failed for %s: %w", pane, err)
	}
	if _, err := a.runCmd(ctx, "tmux", "send-keys", "-t", pane, "Enter"); err != nil {
		return fmt.Errorf("actuators: tmux send-keys Enter failed for %s: %w", pane, err)
	}

	a.audit(map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339),
		"action":     "compact",
		"session_id": s.ID,
		"method":     steward.MethodTmux.String(),
		"reason":     reason,
		"sent":       true,
	})
	return nil
}

// --- MethodPtySock ---

const maxFrameBytes = 1 << 20 // 1 MiB, matches the protocol's stated max.

// ctrlMsg is the small control-JSON envelope carried by kind-1 frames.
type ctrlMsg struct {
	T     string `json:"t"`
	Token string `json:"token,omitempty"`
}

// encodeFrame builds one wire frame: 4-byte big-endian payload length + 1
// byte kind, then the payload.
func encodeFrame(kind byte, payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	buf[4] = kind
	copy(buf[5:], payload)
	return buf
}

// pdFrame is one decoded frame, used by decodeFrames and readFrame.
type pdFrame struct {
	Kind    byte
	Payload []byte
}

// readFrame reads exactly one frame from r, enforcing maxFrameBytes.
func readFrame(r *bufio.Reader) (pdFrame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return pdFrame{}, err
	}
	length := binary.BigEndian.Uint32(header[0:4])
	if length > maxFrameBytes {
		return pdFrame{}, fmt.Errorf("actuators: ptysock frame too large: %d bytes", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return pdFrame{}, err
		}
	}
	return pdFrame{Kind: header[4], Payload: payload}, nil
}

// decodeFrames parses every frame out of a byte slice — a test helper so
// assertions can check the exact bytes an actuator wrote to a conn.
func decodeFrames(data []byte) ([]pdFrame, error) {
	r := bufio.NewReader(bytes.NewReader(data))
	var out []pdFrame
	for {
		f, err := readFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, f)
	}
}

// rosterDoc mirrors the daemon roster's JSON shape enough to look up one
// worker's ptyAuth by sessionId.
type rosterDoc struct {
	Workers map[string]struct {
		SessionID string `json:"sessionId"`
		PtyAuth   string `json:"ptyAuth"`
		PtySock   string `json:"ptySock"`
	} `json:"workers"`
}

// lookupPtyAuth reads rosterPath and returns the ptyAuth secret for the
// worker whose sessionId matches sessionID. The secret is read locally and
// used only for the handshake — callers must never log or audit it.
func (a *osActuator) lookupPtyAuth(sessionID string) (string, error) {
	data, err := os.ReadFile(a.rosterPath)
	if err != nil {
		return "", fmt.Errorf("actuators: roster read failed: %w", err)
	}
	var doc rosterDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("actuators: roster parse failed: %w", err)
	}
	for _, w := range doc.Workers {
		if w.SessionID == sessionID {
			if w.PtyAuth == "" {
				return "", fmt.Errorf("actuators: roster entry for session %s has no ptyAuth", sessionID)
			}
			return w.PtyAuth, nil
		}
	}
	return "", fmt.Errorf("actuators: no roster entry for session %s", sessionID)
}

func writeCtrlFrame(conn net.Conn, msg ctrlMsg) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = conn.Write(encodeFrame(1, b))
	return err
}

// ptySockHandshake performs the pong/auth handshake and reads frames until
// it sees "live", replying to any "ping" along the way (a ping not answered
// gets the connection dropped by the server). Screen-replay kind-0 frames
// and "hello" are discarded. Any read/write error or explicit rejection
// propagates so the caller marks the session do-not-touch.
func ptySockHandshake(ctx context.Context, conn net.Conn, ptyAuth string) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if err := writeCtrlFrame(conn, ctrlMsg{T: "pong"}); err != nil {
		return fmt.Errorf("actuators: ptysock initial pong failed: %w", err)
	}
	if err := writeCtrlFrame(conn, ctrlMsg{T: "auth", Token: ptyAuth}); err != nil {
		return fmt.Errorf("actuators: ptysock auth failed: %w", err)
	}

	r := bufio.NewReader(conn)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		f, err := readFrame(r)
		if err != nil {
			return fmt.Errorf("actuators: ptysock handshake read failed: %w", err)
		}
		if f.Kind != 1 {
			continue // kind-0 screen replay — discard
		}
		var msg ctrlMsg
		if err := json.Unmarshal(f.Payload, &msg); err != nil {
			continue // malformed control frame — ignore, keep reading
		}
		switch msg.T {
		case "ping":
			if err := writeCtrlFrame(conn, ctrlMsg{T: "pong"}); err != nil {
				return fmt.Errorf("actuators: ptysock ping reply failed: %w", err)
			}
		case "live":
			return nil
		case "hello":
			// expected preamble — keep reading
		default:
			// unknown control frame — ignore rather than abort; the
			// protocol may grow fields we don't act on yet.
		}
	}
}

// compactPtySock connects to the bg-pty-host worker, performs the framed
// handshake, and injects "/compact\r" as a single kind-0 data frame.
func (a *osActuator) compactPtySock(ctx context.Context, s steward.SessionState, reason string) error {
	if s.PtySock == "" {
		return fmt.Errorf("actuators: session %s has no ptySock", s.ID)
	}
	ptyAuth, err := a.lookupPtyAuth(s.ID)
	if err != nil {
		return err
	}

	a.audit(map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339),
		"action":     "compact",
		"session_id": s.ID,
		"method":     steward.MethodPtySock.String(),
		"reason":     reason,
		"sent":       false,
	})

	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	conn, err := a.dialUnix(dctx, s.PtySock)
	if err != nil {
		return fmt.Errorf("actuators: ptysock dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := ptySockHandshake(dctx, conn, ptyAuth); err != nil {
		return err
	}

	if _, err := conn.Write(encodeFrame(0, []byte("/compact\r"))); err != nil {
		return fmt.Errorf("actuators: ptysock inject failed: %w", err)
	}

	a.audit(map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339),
		"action":     "compact",
		"session_id": s.ID,
		"method":     steward.MethodPtySock.String(),
		"reason":     reason,
		"sent":       true,
	})
	return nil
}

// audit appends one JSON line to auditPath (mkdir -p, append-only). Errors
// are swallowed: the audit trail is best-effort and must never block or
// fail a compaction attempt. NEVER pass ptyAuth or any other secret in rec.
func (a *osActuator) audit(rec map[string]any) {
	if a.auditPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.auditPath), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(a.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	data = append(data, '\n')
	_, _ = f.Write(data)
}

// ---------------------------------------------------------------------------
// busNotifier — steward.Notifier
// ---------------------------------------------------------------------------

// busNotifier publishes a human-facing notice on nervous-bus. Fire-and-
// forget: PublishAsync errors are swallowed, never returned, matching the
// existing host-hook convention (internal/hooks/nbuspub).
type busNotifier struct{}

func newBusNotifier() *busNotifier { return &busNotifier{} }

func (n *busNotifier) Notify(_ context.Context, level steward.Level, sessionID, message string) error {
	payload, err := json.Marshal(map[string]any{
		"level":      level.String(),
		"session_id": sessionID,
		"message":    message,
		"ts":         time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil // swallow — notify is best-effort
	}
	_ = nbuspub.PublishAsync("bus.steward.notify.v1", nbuspub.SourceTag("steward"), string(payload))
	return nil
}

// ---------------------------------------------------------------------------
// fileBudgetWriter / fileGateWriter — atomic JSON writers
// ---------------------------------------------------------------------------

// atomicWriteJSON marshals v as indented JSON and writes it to path
// atomically: write to a temp file in the same directory, then os.Rename —
// a reader must never observe a half-written file.
func atomicWriteJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("actuators: mkdir %s failed: %w", dir, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("actuators: marshal failed: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("actuators: tempfile create failed: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("actuators: tempfile write failed: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("actuators: tempfile close failed: %w", closeErr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("actuators: rename into place failed: %w", err)
	}
	return nil
}

// fileBudgetWriter persists the public budget.json contract atomically.
type fileBudgetWriter struct {
	path string // default ~/.claude/steward/budget.json
}

func newFileBudgetWriter() *fileBudgetWriter {
	return &fileBudgetWriter{path: filepath.Join(defaultStewardDir(), "budget.json")}
}

func (w *fileBudgetWriter) Write(_ context.Context, doc steward.BudgetDoc) error {
	return atomicWriteJSON(w.path, doc)
}

// gateDoc is the small public document at gate.json.
type gateDoc struct {
	Gate      string `json:"gate"`
	Level     string `json:"level"`
	UpdatedAt string `json:"updated_at"`
}

// fileGateWriter publishes the fan-out gate the rules engine reads.
type fileGateWriter struct {
	path string // default ~/.claude/steward/gate.json
}

func newFileGateWriter() *fileGateWriter {
	return &fileGateWriter{path: filepath.Join(defaultStewardDir(), "gate.json")}
}

func (w *fileGateWriter) SetGate(_ context.Context, gate steward.FanoutGate, level steward.Level) error {
	doc := gateDoc{
		Gate:      string(gate),
		Level:     level.String(),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return atomicWriteJSON(w.path, doc)
}

// ---------------------------------------------------------------------------
// default paths
// ---------------------------------------------------------------------------

func defaultStewardDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "steward")
}

func defaultRosterPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "daemon", "roster.json")
}
