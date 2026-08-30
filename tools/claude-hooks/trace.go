package main

// Invocation tracer: every claude-hook-fast run appends one JSONL record to
// ~/.claude/hooks/log/hook-trace.jsonl capturing what the hook was asked,
// what it wrote to stdout/stderr, its exit code, and duration. Purpose:
// settle "a hook ate the assistant's message" disputes with evidence —
// if MessageDisplay never traced a given message, the drop happened in the
// client before any hook ran; if it did, the hook layer saw it.
//
// Fire-and-forget like hooklog: never crashes, never blocks the hook result.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rotation: hook-trace.jsonl is appended by every hook invocation of every
// live session, concurrently — measured 199MB / 1.35M lines with 25 days of
// unrotated growth. Rotation must never fail the hook: a lost race (another
// process rotated first) or a failed rename is swallowed, not reported.
//
// Each claude-hook-fast subcommand is its own short-lived process (see
// main.go), so there is no persistent counter to sample "1-in-N writes"
// across invocations — an in-process counter would just always be 1. A
// single os.Stat per invocation is cheap next to the fork/exec that already
// happened to get here, so traceMaybeRotate just checks every time instead.
const (
	traceMaxSize       = 50 * 1024 * 1024    // rotate once the live file exceeds this
	traceMaxArchives   = 3                   // keep at most this many rotated files...
	traceMaxArchiveAge = 14 * 24 * time.Hour // ...and never one older than this
)

var (
	traceStart      time.Time
	traceCmd        string
	traceRealStdout *os.File
	traceRealStderr *os.File
	traceOutBuf     *lockedBuf
	traceErrBuf     *lockedBuf
	traceOutW       *os.File
	traceErrW       *os.File
	traceCopyDone   sync.WaitGroup
	traceFinished   bool
	traceExtra      map[string]any
	traceExtraMu    sync.Mutex
)

type lockedBuf struct {
	mu sync.Mutex
	b  []byte
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	return len(p), nil
}

func (l *lockedBuf) snapshot() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b...)
}

func tracePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/hook-trace.jsonl"
	}
	dir := filepath.Join(home, ".claude", "hooks", "log")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "hook-trace.jsonl")
}

// traceInit tees stdout+stderr so the final record carries exactly what the
// hook emitted. Must run first thing in main, before any handler writes.
func traceInit(cmd string) {
	traceStart = time.Now()
	traceCmd = cmd
	traceExtra = map[string]any{}

	tee := func(real *os.File) (*os.File, *lockedBuf) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, nil // tee unavailable; trace still records exit/duration
		}
		buf := &lockedBuf{}
		traceCopyDone.Add(1)
		go func() {
			defer traceCopyDone.Done()
			_, _ = io.Copy(io.MultiWriter(real, buf), r)
		}()
		return w, buf
	}

	traceRealStdout, traceRealStderr = os.Stdout, os.Stderr
	if w, b := tee(os.Stdout); w != nil {
		os.Stdout, traceOutW, traceOutBuf = w, w, b
	}
	if w, b := tee(os.Stderr); w != nil {
		os.Stderr, traceErrW, traceErrBuf = w, w, b
	}
}

// traceNote attaches a handler-supplied key/value to this invocation's record
// (e.g. message-display previews). Safe from any goroutine.
func traceNote(key string, val any) {
	traceExtraMu.Lock()
	defer traceExtraMu.Unlock()
	if traceExtra != nil {
		traceExtra[key] = val
	}
}

const tracePreviewLen = 400

func preview(b []byte) string {
	s := string(b)
	if len(s) > tracePreviewLen {
		return s[:tracePreviewLen] + "…"
	}
	return s
}

// traceFinish flushes the tees and appends the record. Idempotent; called
// from hookExit and at the end of main.
func traceFinish(exitCode int) {
	if traceFinished || traceCmd == "" {
		return
	}
	traceFinished = true

	if traceOutW != nil {
		os.Stdout = traceRealStdout
		_ = traceOutW.Close()
	}
	if traceErrW != nil {
		os.Stderr = traceRealStderr
		_ = traceErrW.Close()
	}
	waitCh := make(chan struct{})
	go func() { traceCopyDone.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(200 * time.Millisecond):
	}

	rec := map[string]any{
		"ts":     traceStart.UTC().Format(time.RFC3339Nano),
		"cmd":    traceCmd,
		"pid":    os.Getpid(),
		"dur_ms": time.Since(traceStart).Milliseconds(),
		"exit":   exitCode,
	}
	if traceOutBuf != nil {
		out := traceOutBuf.snapshot()
		rec["stdout_bytes"] = len(out)
		if len(out) > 0 {
			rec["stdout"] = preview(out)
		}
	}
	if traceErrBuf != nil {
		errb := traceErrBuf.snapshot()
		rec["stderr_bytes"] = len(errb)
		if len(errb) > 0 {
			rec["stderr"] = preview(errb)
		}
	}
	traceExtraMu.Lock()
	for k, v := range traceExtra {
		rec[k] = v
	}
	traceExtraMu.Unlock()

	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	path := tracePath()
	traceMaybeRotate(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

// traceMaybeRotate checks whether the live trace file has grown past
// traceMaxSize and, if so, renames it out of the way so the next append
// recreates it fresh. Every concurrent hook invocation calls this
// independently, so losing the rename race (another process already
// rotated) is the expected common case, not an error.
func traceMaybeRotate(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < traceMaxSize {
		return
	}
	ext := filepath.Ext(path)
	archive := strings.TrimSuffix(path, ext) + "-" + time.Now().UTC().Format("20060102-150405") + ext
	if err := os.Rename(path, archive); err != nil {
		// Someone else rotated first, or the rename otherwise failed —
		// either way the hook must proceed; the next OpenFile recreates
		// whatever file is (or isn't) there.
		return
	}
	tracePruneArchives(filepath.Dir(path))
}

// tracePruneArchives deletes rotated hook-trace-*.jsonl files beyond the
// newest traceMaxArchives, and any archive older than traceMaxArchiveAge
// regardless of count. Best-effort: errors are swallowed, never surfaced.
func tracePruneArchives(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type archiveFile struct {
		path string
		mod  time.Time
	}
	var archives []archiveFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "hook-trace-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		archives = append(archives, archiveFile{filepath.Join(dir, name), info.ModTime()})
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].mod.After(archives[j].mod) })

	cutoff := time.Now().Add(-traceMaxArchiveAge)
	for i, a := range archives {
		if i < traceMaxArchives && a.mod.After(cutoff) {
			continue
		}
		_ = os.Remove(a.path)
	}
}
