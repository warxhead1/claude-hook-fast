package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInbox appends JSONL message lines to <dir>/inbox.jsonl in the shape
// peer_bus_bridge.go produces.
func writeInbox(t *testing.T, dir string, bodies ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, inboxFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open inbox: %v", err)
	}
	defer func() { _ = f.Close() }()
	for _, b := range bodies {
		rec, _ := json.Marshal(map[string]any{
			"ts":   "2026-05-29T00:00:00Z",
			"from": map[string]string{"session_id": "peer-abc"},
			"to":   map[string]string{"session_id": "me"},
			"body": b,
			"kind": "text",
		})
		if _, err := f.Write(append(rec, '\n')); err != nil {
			t.Fatalf("write inbox: %v", err)
		}
	}
}

func TestDrainUnreadInbox_EmptyWhenNoFile(t *testing.T) {
	if got := drainUnreadInbox(t.TempDir()); got != nil {
		t.Fatalf("expected nil for missing inbox, got %v", got)
	}
}

func TestDrainUnreadInbox_ReadsThenAdvances(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "first", "second")

	got := drainUnreadInbox(dir)
	if len(got) != 2 || got[0].Body != "first" || got[1].Body != "second" {
		t.Fatalf("first drain wrong: %+v", got)
	}
	// Offset advanced → second drain sees nothing new.
	if again := drainUnreadInbox(dir); again != nil {
		t.Fatalf("expected no unread on second drain, got %+v", again)
	}
	// A newly delivered message IS picked up.
	writeInbox(t, dir, "third")
	if more := drainUnreadInbox(dir); len(more) != 1 || more[0].Body != "third" {
		t.Fatalf("expected only the new message, got %+v", more)
	}
}

func TestDrainUnreadInbox_ResetsOnTruncate(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "a", "b")
	drainUnreadInbox(dir) // advance offset past both
	// Simulate rotation: truncate the file below the saved offset.
	if err := os.WriteFile(filepath.Join(dir, inboxFile), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	writeInbox(t, dir, "fresh")
	got := drainUnreadInbox(dir)
	if len(got) != 1 || got[0].Body != "fresh" {
		t.Fatalf("expected reset+fresh after truncation, got %+v", got)
	}
}

func TestEmitSessionStartInjection_AdditionalContext(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "apply the repoint spec")

	var buf bytes.Buffer
	emitSessionStartInjection(&buf, dir)

	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("output not valid JSON (%v): %q", err, buf.String())
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("wrong hookEventName: %q", out.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "apply the repoint spec") {
		t.Fatalf("body not injected: %q", out.HookSpecificOutput.AdditionalContext)
	}
}

func TestEmitSessionStartInjection_SilentWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	emitSessionStartInjection(&buf, t.TempDir()) // no inbox
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty inbox, got %q", buf.String())
	}
}

func TestEmitStopInjection_BlocksWithReason(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "heads up: I changed schemas/X")

	var buf bytes.Buffer
	emitStopInjection(&buf, dir, []byte(`{}`))

	var out struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("output not valid JSON (%v): %q", err, buf.String())
	}
	if out.Decision != "block" {
		t.Fatalf("expected decision=block, got %q", out.Decision)
	}
	if !strings.Contains(out.Reason, "heads up: I changed schemas/X") {
		t.Fatalf("message not in reason: %q", out.Reason)
	}
}

func TestEmitStopInjection_NoLoop(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "msg")

	var first bytes.Buffer
	emitStopInjection(&first, dir, []byte(`{}`))
	if first.Len() == 0 {
		t.Fatal("first stop should have blocked")
	}
	// Same messages must NOT re-block (offset advanced) → no infinite loop.
	var second bytes.Buffer
	emitStopInjection(&second, dir, []byte(`{}`))
	if second.Len() != 0 {
		t.Fatalf("second stop re-blocked on same messages: %q", second.String())
	}
}

func TestEmitStopInjection_RespectsStopHookActive(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "msg")

	var buf bytes.Buffer
	emitStopInjection(&buf, dir, []byte(`{"stop_hook_active":true}`))
	if buf.Len() != 0 {
		t.Fatalf("should not block when stop_hook_active, got %q", buf.String())
	}
}

func TestInjection_KillSwitch(t *testing.T) {
	dir := t.TempDir()
	writeInbox(t, dir, "msg")
	t.Setenv("HEARTH_HOOK_INBOX_INJECT", "0")

	var a, b bytes.Buffer
	emitSessionStartInjection(&a, dir)
	emitStopInjection(&b, dir, []byte(`{}`))
	if a.Len() != 0 || b.Len() != 0 {
		t.Fatalf("kill-switch did not disable injection: ss=%q stop=%q", a.String(), b.String())
	}
}
