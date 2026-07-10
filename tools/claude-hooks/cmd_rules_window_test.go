package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The 2026-07-09 wave lockout: the legacy bare counter incremented on blocked
// attempts and every write refreshed the mtime the TTL reset keyed on, so a
// blocked agent retrying could never recover. Window counters store one epoch
// per ALLOWED invocation and prune per-entry; these tests lock the helpers'
// contract (staging/flush ordering is exercised by the conformance script).

func TestPruneWindowAgesEntriesIndividually(t *testing.T) {
	now := time.Now()
	ttl := 15 * time.Minute
	entries := []int64{
		now.Add(-16 * time.Minute).Unix(), // aged out
		now.Add(-10 * time.Minute).Unix(), // live
		now.Add(-1 * time.Minute).Unix(),  // live
	}
	got := pruneWindow(entries, ttl, now)
	if len(got) != 2 {
		t.Fatalf("pruneWindow kept %d entries, want 2 (only the >15m entry ages out)", len(got))
	}
	if got[0] != entries[1] || got[1] != entries[2] {
		t.Errorf("pruneWindow kept wrong entries: %v", got)
	}
}

func TestPruneWindowNoTTLFailsOpen(t *testing.T) {
	now := time.Now()
	if got := pruneWindow([]int64{now.Unix()}, 0, now); len(got) != 0 {
		t.Errorf("window with ttl<=0 must fail open (empty), got %v", got)
	}
}

func TestReadWindowLegacyBareCountIsAmnestied(t *testing.T) {
	dir := t.TempDir()
	// Legacy state: a bare count. Parses as epoch 9 (1970) and must prune out,
	// so an upgraded deployment starts from an empty window.
	if err := os.WriteFile(filepath.Join(dir, "cold_starts"), []byte("9"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := readWindow(dir, "cold_starts")
	got := pruneWindow(entries, 15*time.Minute, time.Now())
	if len(got) != 0 {
		t.Errorf("legacy bare count must prune to an empty window, got %v", got)
	}
}

func TestReadWindowCorruptContentIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "w"), []byte("not-a-number\n123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readWindow(dir, "w"); got != nil {
		t.Errorf("corrupt window content must read as empty, got %v", got)
	}
}

func TestWindowWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	want := []int64{now.Add(-5 * time.Minute).Unix(), now.Unix()}
	writeWindow(dir, "w", want)
	got := readWindow(dir, "w")
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("round trip mismatch: wrote %v read %v", want, got)
	}
}

func TestResetRuleCounterClearsWindow(t *testing.T) {
	dir := t.TempDir()
	writeWindow(dir, "w", []int64{time.Now().Unix()})
	resetRuleCounter(dir, ruleCfg{State: stateCfg{Counter: "w", Window: true}})
	if got := readWindow(dir, "w"); len(got) != 0 {
		t.Errorf("resetRuleCounter must clear a window counter, got %v", got)
	}
}
