package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetTraceState clears the package-level trace globals between test cases;
// traceInit/traceFinish are written for one-shot-per-process use, so tests
// that call them repeatedly must reset by hand.
func resetTraceState() {
	traceStart = time.Time{}
	traceCmd = ""
	traceRealStdout = nil
	traceRealStderr = nil
	traceOutBuf = nil
	traceErrBuf = nil
	traceOutW = nil
	traceErrW = nil
	traceCopyDone = sync.WaitGroup{}
	traceFinished = false
	traceExtra = nil
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

// TestTraceWriteAppendsRecord confirms the basic init/finish path still
// appends one well-formed JSONL record per invocation.
func TestTraceWriteAppendsRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	resetTraceState()
	defer resetTraceState()

	traceInit("test-cmd")
	os.Stdout.WriteString("") // keep stdout live; traceInit already teed it
	traceFinish(0)

	path := filepath.Join(dir, ".claude", "hooks", "log", "hook-trace.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 record, got %d: %q", len(lines), data)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("record not valid JSON: %v", err)
	}
	if rec["cmd"] != "test-cmd" {
		t.Errorf("cmd = %v, want test-cmd", rec["cmd"])
	}
}

// TestTraceMaybeRotateBelowThreshold confirms a small file is left alone.
func TestTraceMaybeRotateBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-trace.jsonl")
	if err := os.WriteFile(path, []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	traceMaybeRotate(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should be untouched: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "hook-trace-*.jsonl"))
	if len(matches) != 0 {
		t.Fatalf("no archive expected, found %v", matches)
	}
}

// TestTraceMaybeRotateOverThreshold confirms a file over traceMaxSize gets
// renamed out of the way and the original path is free for recreation.
func TestTraceMaybeRotateOverThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-trace.jsonl")
	big := make([]byte, traceMaxSize+1)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}

	traceMaybeRotate(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original path should be gone after rotation, stat err = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "hook-trace-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want 1 archive, got %v", matches)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(big)) {
		t.Errorf("archive size = %d, want %d", info.Size(), len(big))
	}

	// Next append recreates the live path fresh.
	if err := os.WriteFile(path, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, path); got != 1 {
		t.Errorf("recreated file has %d lines, want 1", got)
	}
}

// TestTraceMaybeRotateConcurrentIsTolerant simulates the real deployment
// shape: many independent processes racing traceMaybeRotate against the same
// oversized file. Exactly one rename can win; every other call must return
// without panicking or erroring the caller (rotation failure must never fail
// the hook).
func TestTraceMaybeRotateConcurrentIsTolerant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-trace.jsonl")
	big := make([]byte, traceMaxSize+1)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			// Must not panic even though only one goroutine's os.Rename
			// can actually succeed.
			traceMaybeRotate(path)
		}()
	}
	wg.Wait()

	matches, err := filepath.Glob(filepath.Join(dir, "hook-trace-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly 1 archive from the race, got %v", matches)
	}
}

// TestTracePruneArchivesKeepsMaxAndDropsOld covers both prune rules: beyond
// traceMaxArchives most-recent, and anything older than traceMaxArchiveAge,
// regardless of count.
func TestTracePruneArchivesKeepsMaxAndDropsOld(t *testing.T) {
	dir := t.TempDir()

	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}

	now := time.Now()
	// More than traceMaxArchives recent files, plus one ancient one.
	recentNames := []string{
		"hook-trace-20260101-000001.jsonl",
		"hook-trace-20260101-000002.jsonl",
		"hook-trace-20260101-000003.jsonl",
		"hook-trace-20260101-000004.jsonl",
		"hook-trace-20260101-000005.jsonl",
	}
	for i, n := range recentNames {
		mk(n, now.Add(-time.Duration(i)*time.Minute))
	}
	ancient := mk("hook-trace-20200101-000000.jsonl", now.Add(-30*24*time.Hour))
	// Non-archive files must never be touched.
	other := mk("hook-trace.jsonl", now)

	tracePruneArchives(dir)

	if _, err := os.Stat(ancient); !os.IsNotExist(err) {
		t.Errorf("ancient archive should be pruned by age, stat err = %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("live trace file must never be pruned: %v", err)
	}

	remaining, err := filepath.Glob(filepath.Join(dir, "hook-trace-2026*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != traceMaxArchives {
		t.Fatalf("want %d surviving archives, got %d: %v", traceMaxArchives, len(remaining), remaining)
	}
}
