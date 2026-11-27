package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

// sessionBinding matches the JSON file written by Claude Code's needs_you.py
// bind() and read by Hearth's crates/hearth-session/src/session_bindings.rs
// to route terminal text and check idle_now.
type sessionBinding struct {
	SessionID    string  `json:"session_id"`
	OrcaTerminal string  `json:"orca_terminal"`
	TmuxPane     string  `json:"tmux_pane"`
	CWD          string  `json:"cwd"`
	PID          int     `json:"pid"`
	TS           float64 `json:"ts"`
	Turn         string  `json:"turn,omitempty"`
	TurnTS       float64 `json:"turn_ts,omitempty"`
	Transcript   string  `json:"transcript,omitempty"`
}

func hearthBindingsDir() string {
	if d := os.Getenv("HEARTH_BINDINGS_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".local", "state", "hearth", "session-bindings")
}

// writeSessionBinding atomically writes ~/.local/state/hearth/session-bindings/<sid>.json
// when the session runs inside an Orca terminal or tmux pane. Best-effort: all
// errors are swallowed per fail-open hook discipline.
func writeSessionBinding(payload orderedMap, turn string) {
	sid := payload.GetString("conversationId")
	if sid == "" || strings.ContainsAny(sid, "/.") {
		return
	}
	orcaTerm := event.OrcaTerminalHandle()
	tmuxPane := os.Getenv("TMUX_PANE")
	if orcaTerm == "" && tmuxPane == "" {
		return
	}
	dir := hearthBindingsDir()
	path := filepath.Join(dir, sid+".json")

	now := float64(time.Now().UnixNano()) / 1e9
	rec := sessionBinding{
		SessionID:    sid,
		OrcaTerminal: orcaTerm,
		TmuxPane:     tmuxPane,
		CWD:          effectiveCWD(payload),
		PID:          os.Getppid(),
		TS:           now,
	}

	// Preserve prior turn/turn_ts/transcript if already present.
	if raw, err := os.ReadFile(path); err == nil {
		var old sessionBinding
		if json.Unmarshal(raw, &old) == nil {
			rec.Turn = old.Turn
			rec.TurnTS = old.TurnTS
			rec.Transcript = old.Transcript
		}
	}
	if tp := payload.GetString("transcriptPath"); tp != "" {
		rec.Transcript = tp
	}
	if turn != "" {
		rec.Turn = turn
		rec.TurnTS = now
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	writeAtomicFile(dir, path, data)
}

func writeAtomicFile(dir, dst string, data []byte) {
	tmp, err := os.CreateTemp(dir, ".agy-tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
	}
}
