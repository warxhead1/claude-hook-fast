// cmd_session_registry.go — hygiene for the CC-owned per-process registry
// ~/.claude/sessions/<pid>.json (fields: pid, sessionId, cwd, procStart,
// version, status, updatedAt, ...). Claude Code itself writes this file;
// everything here only ever merges known keys into it and otherwise
// preserves whatever CC put there. Two gaps this closes:
//
//  1. Ghost files: when a claude process exits, its registry file is left
//     behind forever, still claiming a live sessionId. SessionEnd
//     (nbus-session-end, see cmd_nbus.go's cmdNbusSessionEnd) tombstones
//     the file for the ending session by merging status/endedAt/endReason
//     — never deletes it, since CC may still read it.
//  2. Stale sessionId: older CC builds (2.1.205/2.1.209) never rewrite the
//     file when the session id rotates mid-process (/clear, resume), so a
//     live process's registry file keeps pointing at a dead sessionId.
//     SessionStart (session-init, see cmd_session_init.go's cmdSessionInit)
//     repairs this by walking up to the ancestor claude process's registry
//     file and rewriting sessionId when it doesn't match the fresh
//     SessionStart payload.
//
// FAF discipline throughout: every I/O error is swallowed, nothing here
// ever panics or blocks, and the pane-title reset carries a hard 1s
// exec.CommandContext deadline per repo convention (see hot-path-audit).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// srSessionsDir returns ~/.claude/sessions, or "" when $HOME is unset.
// Tests redirect this via t.Setenv("HOME", ...) rather than a package var,
// matching the established pattern in cmd_kb_prime_siblings.go.
func srSessionsDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// srReadRegistryFile parses a ~/.claude/sessions/<pid>.json file into a
// generic map so every unknown field (CC may add more over time) survives
// a round trip. Returns ok=false on any read/parse error.
func srReadRegistryFile(path string) (map[string]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}
	return raw, true
}

// srWriteRegistryFile writes raw back out. Field order is not preserved
// (Go marshals map keys sorted) but every key — known or not — is.
func srWriteRegistryFile(path string, raw map[string]any) bool {
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false
	}
	return os.WriteFile(path, b, 0644) == nil
}

// srRegistryPID reads the "pid" field as an int; 0 when absent/malformed.
func srRegistryPID(raw map[string]any) int {
	if v, ok := raw["pid"].(float64); ok {
		return int(v)
	}
	return 0
}

// srRegistryString reads a string field; "" when absent/malformed.
func srRegistryString(raw map[string]any, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

// ---------- /proc inspection ----------

// srProcDirExists reports whether /proc/<pid> exists — the cheapest
// liveness check available and immune to cross-user kill(2) EPERM
// ambiguity (unlike syscall.Kill(pid, 0), which treats EPERM as "alive").
func srProcDirExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// srReadProcStartTime returns /proc/<pid>/stat's starttime field (field
// 22 in the `man proc` numbering, i.e. index 19 of the space-split tail
// after the comm field's closing ')') as CC's procStart wrote it: the raw
// string, compared byte-for-byte rather than parsed to an int, since that
// is how the registry file stores it. comm can itself contain spaces or
// parentheses, so this splits on the LAST ')' rather than the first.
func srReadProcStartTime(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+1 >= len(s) {
		return "", false
	}
	tail := strings.Fields(s[idx+1:])
	if len(tail) < 20 {
		return "", false
	}
	return tail[19], true
}

// srIsAncestorPID reports whether candidatePID appears in hookPID's PPid
// chain (walked via /proc/<pid>/status, see readParentPID in
// cmd_session_init.go). Bounded depth guards against a corrupt /proc
// producing a cycle. candidatePID<=0 can never be an ancestor.
func srIsAncestorPID(hookPID, candidatePID int) bool {
	if candidatePID <= 0 {
		return false
	}
	pid := hookPID
	for i := 0; i < 32; i++ {
		ppid := readParentPID(pid)
		if ppid <= 0 {
			return false
		}
		if ppid == candidatePID {
			return true
		}
		if ppid <= 1 {
			return false
		}
		pid = ppid
	}
	return false
}

// ---------- SessionEnd: registry tombstone ----------

// srShouldTombstone decides whether a registry record whose sessionId
// matches the ending session should be marked ended. Three independent
// triggers (any one suffices):
//
//   - the recorded pid is dead (no /proc/<pid> — process fully exited)
//   - the recorded pid is alive but /proc/<pid>/stat's starttime no
//     longer matches the file's procStart (the pid was recycled onto an
//     unrelated process since CC wrote the file)
//   - the recorded pid is an ancestor of this hook process (the claude
//     process is exiting RIGHT NOW — this hook runs as its descendant —
//     so /proc still reports it alive with a matching starttime, but it
//     is unambiguously the process ending)
//
// A pid that is alive, procStart-matching, and NOT an ancestor of this
// hook is never touched, even if its sessionId happens to match — that
// combination means a live, unrelated claude process's registry file
// coincidentally carries the same sessionId string, which should not
// happen in practice but must never be tombstoned if it does.
func srShouldTombstone(pid int, recordedProcStart string, hookPID int) bool {
	if !srProcDirExists(pid) {
		return true
	}
	actual, ok := srReadProcStartTime(pid)
	if !ok || actual != recordedProcStart {
		return true
	}
	if srIsAncestorPID(hookPID, pid) {
		return true
	}
	return false
}

// srTombstoneEndedSession scans dir for ~/.claude/sessions/<pid>.json
// files whose sessionId equals the ending sessionID, tombstones the ones
// srShouldTombstone approves (merging status/endedAt/endReason, never
// deleting), and leaves everything else untouched. Returns the count of
// files updated. Pure aside from the filesystem I/O — dir, hookPID, and
// now are all injected so this is fully testable against a temp dir.
func srTombstoneEndedSession(dir, sessionID, reason string, hookPID int, now time.Time) int {
	if dir == "" || sessionID == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	updated := 0
	nowMs := now.UnixMilli()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, ok := srReadRegistryFile(path)
		if !ok || srRegistryString(raw, "sessionId") != sessionID {
			continue
		}
		if !srShouldTombstone(srRegistryPID(raw), srRegistryString(raw, "procStart"), hookPID) {
			continue
		}
		raw["status"] = "ended"
		raw["endedAt"] = nowMs
		raw["endReason"] = reason
		if srWriteRegistryFile(path, raw) {
			updated++
		}
	}
	return updated
}

// srResetPaneTitle clears the tmux pane title CC set via OSC escapes and
// never resets, leaving ghost titles like "✳ Claude Code" on plain zsh
// panes after the session ends. No-op outside tmux. Every error
// (including timeout) is swallowed — this is pure cosmetics.
func srResetPaneTitle() {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = osexec.CommandContext(ctx, "tmux", "select-pane", "-t", pane, "-T", "").Run() //nolint:gosec
}

// ---------- SessionStart: stale-sessionId repair ----------

// srFindAncestorRegistryFile walks the PPid chain from hookPID upward
// (via readParentPID), returning the path to the first
// ~/.claude/sessions/<pid>.json that exists on disk — the CC-owned
// registry file for the claude process that (transitively) spawned this
// hook. Returns "" when none of the ancestors (up to 32 levels, or until
// pid 1) has a registry file.
func srFindAncestorRegistryFile(dir string, hookPID int) string {
	if dir == "" {
		return ""
	}
	pid := hookPID
	for i := 0; i < 32; i++ {
		ppid := readParentPID(pid)
		if ppid <= 1 {
			return ""
		}
		candidate := filepath.Join(dir, strconv.Itoa(ppid)+".json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		pid = ppid
	}
	return ""
}

// srRepairStaleSessionID merges a rotated session_id into path's registry
// file when its recorded sessionId differs from newSessionID — the
// process is clearly alive (SessionStart just fired for it), so a stale
// sessionId means an old CC build never rewrote the file across /clear or
// resume. Merges sessionId + updatedAt; if status was "ended" (a prior
// SessionEnd tombstoned this same pid+procStart before it was reused by a
// continuing process — shouldn't normally happen, but is cheap to guard),
// resets it to "idle" and drops endedAt/endReason. Never creates the
// file — CC owns creation. Returns whether it modified anything.
func srRepairStaleSessionID(path, newSessionID string, now time.Time) bool {
	if path == "" || newSessionID == "" {
		return false
	}
	raw, ok := srReadRegistryFile(path)
	if !ok {
		return false
	}
	if srRegistryString(raw, "sessionId") == newSessionID {
		return false
	}
	raw["sessionId"] = newSessionID
	raw["updatedAt"] = now.UnixMilli()
	if srRegistryString(raw, "status") == "ended" {
		raw["status"] = "idle"
		delete(raw, "endedAt")
		delete(raw, "endReason")
	}
	return srWriteRegistryFile(path, raw)
}
