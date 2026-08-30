// hearth-loom-hook is the in-container HTTP-push observability shim used
// by both runtimes (Claude Code via .claude/settings.json, opencode via
// the hearth-loom-hooks.js plugin's spawnSync fallback).
//
// One Go binary, multiple subcommands:
//
//	hearth-loom-hook session-start    # SessionStart hook
//	hearth-loom-hook tool-pre         # PreToolUse hook
//	hearth-loom-hook tool-post        # PostToolUse hook
//	hearth-loom-hook stop             # Stop hook
//	hearth-loom-hook subagent-stop    # SubagentStop hook (alias of stop)
//
// Each subcommand reads its hook event from stdin (JSON, schema defined
// by the runtime), enriches with $HEARTH_BEAD_ID + the session-id
// written by the runtime entrypoint to $HEARTH_SESSION_INFO_DIR/session-id
// (claude-entrypoint writes this; opencode plugin propagates the
// sessionID it observes on the bus), and POSTs to the hearth-loom
// gateway at $HEARTH_LOOM_GATEWAY_URL (default
// http://host.docker.internal:5908).
//
// Failure semantics — DO NOT BLOCK THE LOOMIE:
//   - Always exit 0 (a failing hook would kill the runtime; that's a
//     denial-of-service via observability).
//   - 1 second HTTP timeout — if hearth-loom is slow, swallow it.
//   - All errors go to stderr (Claude Code captures hook stderr per-call).
//   - Missing HEARTH_BEAD_ID → log + exit 0 (operator may run a runtime
//     outside the dispatcher for local testing).
//
// Idempotency: hooks may fire twice (network retry, hook chain re-run).
// Each POST carries an event_id (uuid-ish — bead+session+ts+rand) so the
// server can dedupe. The server endpoints are themselves idempotent
// (session/registered uses INSERT ... ON CONFLICT DO NOTHING).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultGateway = "http://host.docker.internal:5908"
	httpTimeout    = 1 * time.Second
	stdinMaxBytes  = 1 << 20 // 1 MiB — Claude Code's PreToolUse payload can be ~100KB
)

// envelope is the shared payload superstructure. Each handler decorates
// it with subcommand-specific fields before POSTing.
type envelope struct {
	EventID   string `json:"event_id"`
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"ts"`
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: hearth-loom-hook <subcommand>")
		return // (always exit 0 via runHook → main)
	}
	// Read stdin ONCE up front: the observability POST and the inbox
	// injection both need it (stdin can only be consumed once).
	stdin, err := readStdinCapped()
	if err != nil {
		warn("read stdin: %v", err)
	}

	sub := os.Args[1]
	switch sub {
	case "session-start":
		runHook(sessionStart, stdin)
		// Surface messages queued while this session was offline/cold.
		emitSessionStartInjection(os.Stdout, sessionInfoDir())
	case "tool-pre":
		runHook(toolEvent("pre"), stdin)
	case "tool-post":
		runHook(toolEvent("post"), stdin)
	case "stop", "subagent-stop":
		runHook(sessionStopped(sub), stdin)
		// Only the main Stop pulls the agent back to consume messages;
		// subagent-stop must not block the subagent mid-flight.
		if sub == "stop" {
			emitStopInjection(os.Stdout, sessionInfoDir(), stdin)
		}
	default:
		fail("unknown subcommand: %s", sub)
	}
}

// runHook wraps a hook handler: lookup bead + session, invoke handler
// with the already-read stdin, and POST. Every failure mode lands on
// stderr — we never propagate non-zero exit codes (see DoS note in
// package doc).
func runHook(h handler, stdin []byte) {
	beadID := os.Getenv("HEARTH_BEAD_ID")
	if beadID == "" {
		warn("HEARTH_BEAD_ID unset — hook no-op (runtime not under hearth-loom dispatcher?)")
		return
	}
	gateway := os.Getenv("HEARTH_LOOM_GATEWAY_URL")
	if gateway == "" {
		gateway = defaultGateway
	}
	sessionID := readSessionID()

	url, body, ok := h(beadID, sessionID, stdin)
	if !ok {
		return
	}
	if err := postWithRetry(gateway+url, body, isSessionRegistered(url)); err != nil {
		warn("POST %s: %v", url, err)
	}
}

// handler returns (relative URL, JSON body bytes, send?). When the third
// value is false the hook silently no-ops (e.g. stdin failed to parse
// in a way the handler can't recover from).
type handler func(beadID, sessionID string, stdin []byte) (string, []byte, bool)

// sessionStart emits POST /api/v2/loomies/{bead}/session/registered.
// Body: {event_id, session_id, runtime, cwd, started_at}.
func sessionStart(beadID, sessionID string, stdin []byte) (string, []byte, bool) {
	var input struct {
		// Claude Code SessionStart hook fields (subset of what
		// settings.json hook handlers see on stdin).
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	_ = json.Unmarshal(stdin, &input) // tolerate empty / malformed — fall back to env

	if input.SessionID != "" {
		sessionID = input.SessionID
	}
	cwd := input.CWD
	if cwd == "" {
		if got, err := os.Getwd(); err == nil {
			cwd = got
		}
	}

	body := struct {
		envelope
		Runtime string `json:"runtime"`
		CWD     string `json:"cwd"`
	}{
		envelope: envelope{EventID: newEventID(), SessionID: sessionID, Timestamp: nowRFC3339()},
		Runtime:  detectRuntime(),
		CWD:      cwd,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		warn("marshal session-start: %v", err)
		return "", nil, false
	}
	return fmt.Sprintf("/api/v2/loomies/%s/session/registered", urlEscape(beadID)), buf, true
}

// toolEvent emits POST /api/v2/loomies/{bead}/tool-event.
// Body: {event_id, session_id, tool, args_size, phase, ts}.
// phase is "pre" or "post".
func toolEvent(phase string) handler {
	return func(beadID, sessionID string, stdin []byte) (string, []byte, bool) {
		var input struct {
			ToolName  string          `json:"tool_name"`
			Tool      string          `json:"tool"` // opencode plugin shape
			ToolInput json.RawMessage `json:"tool_input"`
			SessionID string          `json:"session_id"`
		}
		_ = json.Unmarshal(stdin, &input)
		if input.SessionID != "" {
			sessionID = input.SessionID
		}
		tool := input.ToolName
		if tool == "" {
			tool = input.Tool
		}
		if tool == "" {
			tool = "unknown"
		}

		body := struct {
			envelope
			Tool     string `json:"tool"`
			ArgsSize int    `json:"args_size"`
			Phase    string `json:"phase"`
		}{
			envelope: envelope{EventID: newEventID(), SessionID: sessionID, Timestamp: nowRFC3339()},
			Tool:     tool,
			ArgsSize: len(input.ToolInput),
			Phase:    phase,
		}
		buf, err := json.Marshal(body)
		if err != nil {
			warn("marshal tool-event: %v", err)
			return "", nil, false
		}
		return fmt.Sprintf("/api/v2/loomies/%s/tool-event", urlEscape(beadID)), buf, true
	}
}

// sessionStopped emits POST /api/v2/loomies/{bead}/session/stopped.
// Body: {event_id, session_id, exit_reason, ts, last_tool}.
// origin is "stop" or "subagent-stop" — folded into exit_reason so the
// server doesn't need a separate endpoint.
func sessionStopped(origin string) handler {
	return func(beadID, sessionID string, stdin []byte) (string, []byte, bool) {
		var input struct {
			SessionID  string `json:"session_id"`
			Reason     string `json:"reason"`
			ExitReason string `json:"exit_reason"`
			LastTool   string `json:"last_tool"`
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(stdin, &input)
		if input.SessionID != "" {
			sessionID = input.SessionID
		}
		reason := first(input.ExitReason, input.Reason, input.StopReason, origin)

		body := struct {
			envelope
			ExitReason string `json:"exit_reason"`
			LastTool   string `json:"last_tool,omitempty"`
		}{
			envelope:   envelope{EventID: newEventID(), SessionID: sessionID, Timestamp: nowRFC3339()},
			ExitReason: reason,
			LastTool:   input.LastTool,
		}
		buf, err := json.Marshal(body)
		if err != nil {
			warn("marshal session-stopped: %v", err)
			return "", nil, false
		}
		return fmt.Sprintf("/api/v2/loomies/%s/session/stopped", urlEscape(beadID)), buf, true
	}
}

// readSessionID reads the session id the runtime entrypoint wrote to
// $HEARTH_SESSION_INFO_DIR/session-id. Returns "" if missing — handlers
// fall back to whatever is in the hook event payload.
func readSessionID() string {
	dir := os.Getenv("HEARTH_SESSION_INFO_DIR")
	if dir == "" {
		dir = "/run/loomie"
	}
	data, err := os.ReadFile(filepath.Join(dir, "session-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readStdinCapped reads up to stdinMaxBytes from stdin. We cap to avoid
// pathological hook payloads (Claude Code's MultiEdit can carry huge
// tool_input blobs) chewing up memory. Anything past the cap is
// discarded; we still emit a best-effort event.
func readStdinCapped() ([]byte, error) {
	r := io.LimitReader(os.Stdin, stdinMaxBytes)
	return io.ReadAll(r)
}

// postJSON sends body to url with a 1s timeout. Any non-2xx is treated
// as a soft failure (logged + swallowed).
//
// gosec G107/G704: the URL is constructed from $HEARTH_LOOM_GATEWAY_URL
// (operator env) + a hardcoded /api/v2/loomies/{bead}/* path. The bead
// id is validated by urlEscape (rejects anything outside [a-zA-Z0-9._-])
// so we cannot smuggle a different host or path. SSRF is not a concern
// here — the hook runs inside an isolated container with bridge
// networking and only the daemon is reachable on host.docker.internal.
func postJSON(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body)) // #nosec G107,G704
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) // #nosec G107,G704
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain to allow connection reuse, but we don't care about the body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return errors.New("non-2xx: " + resp.Status)
	}
	return nil
}

func isSessionRegistered(relURL string) bool {
	return strings.HasSuffix(relURL, "/session/registered")
}

func postWithRetry(url string, body []byte, retry bool) error {
	const maxAttempts = 3
	attempts := 1
	if retry {
		attempts = maxAttempts
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * time.Second)
		}
		if err := postJSON(url, body); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// detectRuntime returns a short tag for the runtime that invoked us.
// Claude Code sets CLAUDE_CODE=1 (or equivalent) in its environment;
// opencode runs us only via the plugin spawnSync fallback, which sets
// OPENCODE_PLUGIN=1. When neither is present we fall back to env probe
// on the runtime binary name.
func detectRuntime() string {
	if os.Getenv("OPENCODE_PLUGIN") != "" {
		return "opencode"
	}
	if os.Getenv("CLAUDE_CODE") != "" || os.Getenv("CLAUDE_PROJECT_DIR") != "" {
		return "claude-code"
	}
	if v := os.Getenv("HEARTH_RUNTIME_ID"); v != "" {
		return v
	}
	return "unknown"
}

func newEventID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is exceptionally rare; fall back to a
		// timestamp-only id rather than panic.
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// urlEscape is a tiny path-segment escape. We DO NOT use url.PathEscape
// because bead ids are guaranteed to be in [a-zA-Z0-9._-] (validated at
// creation) and PathEscape adds %-encoding that breaks chi's path
// matcher for some characters that are actually safe. If a bead id
// contains anything weird, we drop the call (the server would 404).
func urlEscape(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return ""
		}
	}
	return s
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[hearth-loom-hook] "+format+"\n", args...)
}

func fail(format string, args ...any) {
	warn(format, args...)
	// NB: always exit 0 — runtime must not be killed by hook failure.
}
