package main

// coord_common.go — shared helpers for coord-wake and coord-status
// (docs/coordinator-inbox.md, component A): state dir layout, the orca
// runner, the run-current identity cache, the lane ledger, wake.log, and the
// ancestor-liveness probe.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// coordMsg is one orchestration message as emitted by `check --json`.
type coordMsg struct {
	ID        string  `json:"id"`
	RunID     string  `json:"run_id"`
	From      string  `json:"from_handle"`
	To        string  `json:"to_handle"`
	Subject   string  `json:"subject"`
	Body      string  `json:"body"`
	Type      string  `json:"type"`
	Priority  string  `json:"priority"`
	ThreadID  *string `json:"thread_id"`
	Payload   string  `json:"payload"`
	CreatedAt string  `json:"created_at"`
}

type coordCheckResp struct {
	OK     bool `json:"ok"`
	Result struct {
		RunID      string     `json:"runId"`
		DeliveryID string     `json:"deliveryId"`
		Messages   []coordMsg `json:"messages"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// coordPayload is the subset of a message payload the ledger uses.
type coordPayload struct {
	TaskID     string `json:"taskId"`
	DispatchID string `json:"dispatchId"`
	Phase      string `json:"phase"`
}

func (m coordMsg) payload() coordPayload {
	var p coordPayload
	_ = json.Unmarshal([]byte(m.Payload), &p)
	return p
}

// coordStateRoot is ${XDG_STATE_HOME:-~/.local/state}/claude-hook-fast/coord.
func coordStateRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "claude-hook-fast", "coord")
}

func coordSafeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

func coordTrunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// coordOrca runs the orca binary and returns stdout only (the --wait
// keepalive lines go to stderr and are discarded).
func coordOrca(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return out.Bytes(), err
}

// ---------- wake.log ----------

const coordLogMax = 1 << 20

func coordLog(dir, format string, args ...any) {
	if dir == "" {
		return
	}
	p := filepath.Join(dir, "wake.log")
	if st, err := os.Stat(p); err == nil && st.Size() >= coordLogMax {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// ---------- run-current identity cache ----------

type coordRunInfo struct {
	RunID       string `json:"run_id"`
	Coordinator string `json:"coordinator_handle"`
	At          int64  `json:"at"`
}

// coordResolveRun returns the run bound to this terminal and whether this
// terminal is its coordinator. The verdict (including "not coordinator") is
// cached per handle so lanes make at most one run-current call per TTL.
func coordResolveRun(ctx context.Context, cfg coordConfig) (runID string, isCoord bool) {
	cp := filepath.Join(cfg.StateRoot, "run-current-"+coordSafeName(cfg.Handle)+".json")
	if b, err := os.ReadFile(cp); err == nil {
		var ci coordRunInfo
		if json.Unmarshal(b, &ci) == nil && ci.RunID != "" &&
			time.Since(time.Unix(ci.At, 0)) < cfg.CacheTTL {
			return ci.RunID, ci.Coordinator == cfg.Handle
		}
	}
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "run-current", "--json")
	if err != nil {
		return "", false
	}
	var rc struct {
		OK     bool `json:"ok"`
		Result struct {
			Run struct {
				ID          string `json:"id"`
				Coordinator string `json:"coordinator_handle"`
			} `json:"run"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &rc) != nil || !rc.OK || rc.Result.Run.ID == "" {
		return "", false
	}
	ci := coordRunInfo{RunID: rc.Result.Run.ID, Coordinator: rc.Result.Run.Coordinator, At: time.Now().Unix()}
	if b, err := json.Marshal(ci); err == nil {
		_ = os.MkdirAll(cfg.StateRoot, 0o755)
		_ = coordWriteAtomic(cp, b)
	}
	return ci.RunID, ci.Coordinator == cfg.Handle
}

func coordWriteAtomic(path string, b []byte) error {
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- lane ledger ----------

type coordLaneStatus struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	At      string `json:"at"`
}

type coordLane struct {
	Handle        string          `json:"handle"`
	TaskID        string          `json:"task_id,omitempty"`
	DispatchID    string          `json:"dispatch_id,omitempty"`
	Title         string          `json:"title,omitempty"`
	LastStatus    coordLaneStatus `json:"last_status"`
	LastHeartbeat string          `json:"last_heartbeat,omitempty"`
	Phase         string          `json:"phase,omitempty"`
	LastSeen      string          `json:"last_seen"`
	LastActionID  string          `json:"last_actionable_id,omitempty"`
	LastActionTyp string          `json:"last_actionable_type,omitempty"`
}

type coordLedger struct {
	RunID   string                `json:"run_id"`
	Updated string                `json:"updated"`
	Lanes   map[string]*coordLane `json:"lanes"`
}

func coordLoadLedger(dir, runID string) *coordLedger {
	l := &coordLedger{RunID: runID, Lanes: map[string]*coordLane{}}
	if b, err := os.ReadFile(filepath.Join(dir, "lanes.json")); err == nil {
		_ = json.Unmarshal(b, l)
		if l.Lanes == nil {
			l.Lanes = map[string]*coordLane{}
		}
	}
	l.RunID = runID
	return l
}

func (l *coordLedger) save(dir string) {
	l.Updated = time.Now().UTC().Format(time.RFC3339)
	if b, err := json.MarshalIndent(l, "", "  "); err == nil {
		_ = coordWriteAtomic(filepath.Join(dir, "lanes.json"), b)
	}
}

func (l *coordLedger) lane(m coordMsg) *coordLane {
	ln := l.Lanes[m.From]
	if ln == nil {
		ln = &coordLane{Handle: m.From}
		l.Lanes[m.From] = ln
	}
	p := m.payload()
	if p.TaskID != "" {
		ln.TaskID = p.TaskID
	}
	if p.DispatchID != "" {
		ln.DispatchID = p.DispatchID
	}
	ln.LastSeen = time.Now().UTC().Format(time.RFC3339)
	return ln
}

// recordTelemetry folds a status/heartbeat message into the ledger.
func (l *coordLedger) recordTelemetry(m coordMsg) {
	ln := l.lane(m)
	now := time.Now().UTC().Format(time.RFC3339)
	switch m.Type {
	case "heartbeat":
		ln.LastHeartbeat = now
		if ph := m.payload().Phase; ph != "" {
			ln.Phase = ph
		}
	default:
		ln.LastStatus = coordLaneStatus{Subject: m.Subject, Body: coordTrunc(m.Body, 400), At: now}
	}
}

func (l *coordLedger) recordActionable(m coordMsg) {
	ln := l.lane(m)
	ln.LastActionID, ln.LastActionTyp = m.ID, m.Type
}

// ---------- ancestor liveness ----------

// coordAncestors returns up to depth ancestors of the current process
// (excluding pid 1). A hook may sit under an sh wrapper that outlives
// claude, so the whole chain is watched: any dead ancestor means the session
// is gone.
func coordAncestors(depth int) []int {
	var out []int
	pid := os.Getpid()
	for i := 0; i < depth; i++ {
		pp := readParentPID(pid)
		if pp <= 1 {
			break
		}
		out = append(out, pp)
		pid = pp
	}
	return out
}

func coordPidAlive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z' && s[i+2] != 'X'
}

func coordAllAlive(pids []int) bool {
	for _, p := range pids {
		if !coordPidAlive(p) {
			return false
		}
	}
	return true
}
