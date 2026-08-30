// Directed-message injection.
//
// peer_bus_bridge.go (in cmd/hearth-loom) tails the bus and appends every
// agent.message addressed to this session's id into
// <SessionInfoDir>/inbox.jsonl. This file turns those queued messages into
// context the running agent actually sees, via two Claude Code hook outputs:
//
//	SessionStart → {"hookSpecificOutput":{"hookEventName":"SessionStart",
//	                 "additionalContext": <messages>}}
//	  Catches messages delivered while the session was OFFLINE/cold — they're
//	  surfaced as the session boots.
//
//	Stop        → {"decision":"block","reason": <messages + nudge>}
//	  Catches messages that arrived WHILE the agent was working: when it tries
//	  to end its turn, the block pulls it back to consume them first.
//
// Loop safety: a hook-owned byte offset (inbox.hook-offset) means each message
// is surfaced exactly once — after a Stop block advances the offset, the same
// messages can't re-block. We also honor Claude Code's stop_hook_active flag
// (skip blocking when already mid-cycle) and the runtime's 8-block cap.
//
// Purely additive: when the inbox is empty (or absent — e.g. a host session
// with no bridge), nothing is written to stdout and the hook behaves exactly
// as before. Set HEARTH_HOOK_INBOX_INJECT=0 to disable entirely.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	inboxFile       = "inbox.jsonl"
	inboxOffsetFile = "inbox.hook-offset"
	inboxScanMax    = 1024 * 1024 // 1 MiB per message line
)

// inboxMessage mirrors the record peer_bus_bridge.go writes per line.
type inboxMessage struct {
	From struct {
		SessionID string `json:"session_id"`
	} `json:"from"`
	Body string `json:"body"`
	Kind string `json:"kind"`
	TS   string `json:"ts"`
}

// injectionEnabled reports whether inbox injection is active (default true;
// HEARTH_HOOK_INBOX_INJECT=0 is the kill-switch).
func injectionEnabled() bool {
	return os.Getenv("HEARTH_HOOK_INBOX_INJECT") != "0"
}

// sessionInfoDir resolves the dir holding session-id + inbox.jsonl. Mirrors
// readSessionID's resolution so the inbox sits beside the session-id file.
func sessionInfoDir() string {
	dir := os.Getenv("HEARTH_SESSION_INFO_DIR")
	if dir == "" {
		dir = "/run/loomie"
	}
	return dir
}

// drainUnreadInbox reads inbox.jsonl from the hook's saved offset to EOF,
// returns the parsed messages, and advances the offset past them so each is
// surfaced exactly once. Returns nil when there's nothing new (or no inbox).
func drainUnreadInbox(dir string) []inboxMessage {
	path := filepath.Join(dir, inboxFile)
	stat, err := os.Stat(path)
	if err != nil {
		return nil // no inbox for this session
	}
	offset := readOffset(dir)
	if stat.Size() < offset {
		offset = 0 // truncated / rotated → re-read from the start this pass
	}
	if stat.Size() <= offset {
		return nil // nothing new
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}

	var msgs []inboxMessage
	newOffset := offset
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), inboxScanMax)
	for sc.Scan() {
		line := sc.Bytes()
		newOffset += int64(len(line)) + 1 // +1 for the stripped newline
		var m inboxMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue // tolerate non-message / malformed lines
		}
		if strings.TrimSpace(m.Body) == "" {
			continue
		}
		msgs = append(msgs, m)
	}
	// Advance even when some lines didn't parse — they won't become valid on
	// a later pass, and not advancing would re-scan them forever.
	writeOffset(dir, newOffset)
	return msgs
}

func readOffset(dir string) int64 {
	data, err := os.ReadFile(filepath.Join(dir, inboxOffsetFile))
	if err != nil {
		return 0
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n); err != nil || n < 0 {
		return 0
	}
	return n
}

func writeOffset(dir string, n int64) {
	_ = os.WriteFile(filepath.Join(dir, inboxOffsetFile), []byte(fmt.Sprintf("%d\n", n)), 0o644)
}

// formatInbox renders messages as injectable plain text. Empty when no msgs.
func formatInbox(msgs []inboxMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	noun := "message"
	if len(msgs) > 1 {
		noun = "messages"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📨 %d directed peer %s for this session (delivered via nervous-bus):\n", len(msgs), noun)
	for i, m := range msgs {
		from := m.From.SessionID
		if from == "" {
			from = "(unknown sender)"
		}
		kind := m.Kind
		if kind == "" {
			kind = "text"
		}
		fmt.Fprintf(&b, "\n[%d] from %s (%s):\n%s\n", i+1, from, kind, strings.TrimSpace(m.Body))
	}
	return b.String()
}

// emitSessionStartInjection writes a SessionStart additionalContext object to
// w when queued messages exist; otherwise writes nothing.
func emitSessionStartInjection(w io.Writer, dir string) {
	if !injectionEnabled() {
		return
	}
	msgs := drainUnreadInbox(dir)
	if len(msgs) == 0 {
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": formatInbox(msgs),
		},
	})
}

// emitStopInjection writes a Stop block decision to w when unread messages
// exist, pulling the agent back to consume them before its turn ends. No-op
// when already in a stop-block cycle (stop_hook_active) — the offset advance
// in drainUnreadInbox is the primary guard against re-blocking the same set.
func emitStopInjection(w io.Writer, dir string, stdin []byte) {
	if !injectionEnabled() {
		return
	}
	var in struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	_ = json.Unmarshal(stdin, &in)
	if in.StopHookActive {
		return // let the stop proceed; deliver fresh messages next turn
	}
	msgs := drainUnreadInbox(dir)
	if len(msgs) == 0 {
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"decision": "block",
		"reason":   formatInbox(msgs) + "\nAcknowledge and address the message(s) above, then continue.",
	})
}
