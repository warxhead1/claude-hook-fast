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
	"sync"
	"time"
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
	f, err := os.OpenFile(tracePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}
