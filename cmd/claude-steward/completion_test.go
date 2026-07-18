package main

import (
	"os"
	"path/filepath"
	"testing"
)

// trLine renders one parent-transcript line: a user message carrying a
// tool_result for the given tool_use_id (a subagent completion).
func trLine(id string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]}}` + "\n"
}

func ctAppend(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionTracker_IncrementalPickup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.jsonl")
	if err := os.WriteFile(path, []byte(trLine("toolu_A")), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newCompletionTracker("")
	if !tr.completed(path, "toolu_A") {
		t.Fatal("toolu_A should be completed")
	}
	if tr.completed(path, "toolu_B") {
		t.Fatal("toolu_B is not present yet")
	}
	// Append a second completion; the cursor must pick it up from the appended
	// bytes alone, without rescanning from the top.
	ctAppend(t, path, trLine("toolu_B"))
	if !tr.completed(path, "toolu_B") {
		t.Fatal("toolu_B should be completed after append")
	}
}

func TestCompletionTracker_PartialLineNotCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.jsonl")
	// A complete line, then a partial tool_result line with no trailing newline
	// (a completion Claude Code is still writing).
	partial := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_C","content":"ok"}]}}`
	if err := os.WriteFile(path, []byte(trLine("toolu_A")+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newCompletionTracker("")
	if !tr.completed(path, "toolu_A") {
		t.Fatal("toolu_A (complete line) should be completed")
	}
	if tr.completed(path, "toolu_C") {
		t.Fatal("toolu_C is on an unterminated line and must not count yet")
	}
	// Terminate the line; only now does it count.
	ctAppend(t, path, "\n")
	if !tr.completed(path, "toolu_C") {
		t.Fatal("toolu_C should be completed once its line is newline-terminated")
	}
}

func TestCompletionTracker_IgnoresDispatchAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.jsonl")
	// A tool_use dispatch (not a result) must never read as a completion.
	toolUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_X","name":"Task"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(toolUse), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newCompletionTracker("")
	if tr.completed(path, "toolu_X") {
		t.Fatal("a tool_use dispatch is not a completion")
	}
	// A missing transcript yields false, never a panic.
	if tr.completed(filepath.Join(dir, "nope.jsonl"), "toolu_Y") {
		t.Fatal("missing transcript should report not-completed")
	}
	// Empty path/id are safe no-ops.
	if tr.completed("", "toolu_Z") || tr.completed(path, "") {
		t.Fatal("empty path/id must be false")
	}
}

func TestCompletionTracker_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "parent.jsonl")
	cache := filepath.Join(dir, "completions.json")
	if err := os.WriteFile(transcript, []byte(trLine("toolu_A")), 0o644); err != nil {
		t.Fatal(err)
	}
	// First instance scans the transcript and saves its cursor.
	tr1 := newCompletionTracker(cache)
	if !tr1.completed(transcript, "toolu_A") {
		t.Fatal("toolu_A should be completed on first instance")
	}
	if err := tr1.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache file should exist after save: %v", err)
	}

	// A fresh instance (simulating a restart / --once run) must know toolu_A is
	// completed from the persisted cache, without the transcript being present
	// to rescan — proving the completed set, not just the offset, round-trips.
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	tr2 := newCompletionTracker(cache)
	if !tr2.completed(transcript, "toolu_A") {
		t.Fatal("toolu_A should be known completed from the persisted cache")
	}
}

func TestCompletionTracker_TruncationResets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.jsonl")
	if err := os.WriteFile(path, []byte(trLine("toolu_A")+trLine("toolu_B")), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newCompletionTracker("")
	if !tr.completed(path, "toolu_B") {
		t.Fatal("toolu_B should be completed before truncation")
	}
	// Rotate/truncate the file to a shorter, different content: the cursor must
	// reset and rescan rather than trust its stale offset.
	if err := os.WriteFile(path, []byte(trLine("toolu_D")), 0o644); err != nil {
		t.Fatal(err)
	}
	if !tr.completed(path, "toolu_D") {
		t.Fatal("toolu_D should be found after truncation reset")
	}
}
