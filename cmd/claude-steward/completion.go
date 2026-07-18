// completion.go — subagent-completion tracking for claude-steward.
//
// # Purpose
//
// A Task/Agent subagent leaves an agent-<hex>.jsonl transcript under its
// parent session's subagents/ directory. That file lingers on disk forever, so
// its mere presence — even its mtime — cannot tell a still-running subagent
// from one that finished days ago. The authoritative "it finished" signal is
// in the PARENT session's transcript: when a subagent returns, Claude Code
// records a tool_result bearing the same tool_use_id the subagent's meta.json
// carries. This file consults that signal.
//
// Claude Code's SubagentStart/SubagentStop hooks are NOT usable for this:
// subagent_id is never populated, and SubagentStop reports only the parent's
// identity (parent session_id + parent transcript), so it cannot say WHICH
// subagent stopped. The tool_use_id <-> tool_result pairing is the only
// per-subagent-precise, retroactively-available completion signal.
//
// # Why a cursor
//
// Parent transcripts are append-only JSONL and can be tens of MB on a
// long-lived session. Re-reading each in full every tick would be wasteful, so
// each transcript is scanned incrementally: the first observation reads it
// whole, every later tick reads only the bytes appended since. The set of
// completed tool_use_ids per transcript persists in memory across ticks. This
// is the one piece of cross-tick state the Reader carries; it is a pure cache,
// rebuildable from disk after a restart.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// completionTracker answers "has the subagent identified by this tool_use_id
// completed?" by tailing parent-session transcripts.
//
// The cursor state (per-transcript byte offset + completed tool_use_ids) is
// persisted atomically to path, so a restart or a one-shot (--once/cron) run
// resumes from the last offset instead of re-reading every parent transcript
// from the top — on a busy box that is the difference between a ~9s cold scan
// of ~180 MB and reading only the handful of bytes appended since. An empty
// path disables persistence (in-memory only), which tests use to stay hermetic.
type completionTracker struct {
	mu      sync.Mutex
	cursors map[string]*transcriptCursor
	path    string
	dirty   bool
}

// transcriptCursor is the incremental-scan state for one parent transcript:
// how far we have read, and the tool_use_ids seen completed so far.
type transcriptCursor struct {
	offset    int64
	completed map[string]struct{}
}

// cursorDoc is the on-disk form of a transcriptCursor (a set serializes as a
// list). The whole file is map[transcriptPath]cursorDoc.
type cursorDoc struct {
	Offset int64    `json:"offset"`
	IDs    []string `json:"ids"`
}

// newCompletionTracker builds a tracker persisted at path, loading any prior
// cursor state. An empty path means in-memory only (no load, no save).
func newCompletionTracker(path string) *completionTracker {
	t := &completionTracker{cursors: map[string]*transcriptCursor{}, path: path}
	t.load()
	return t
}

// load reads the persisted cursor state, tolerating a missing/corrupt file by
// starting empty — a lost cache only costs one cold rescan.
func (c *completionTracker) load() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path) //nolint:gosec // operator-owned path
	if err != nil {
		return
	}
	var docs map[string]cursorDoc
	if err := json.Unmarshal(data, &docs); err != nil {
		return
	}
	for path, doc := range docs {
		cur := &transcriptCursor{offset: doc.Offset, completed: map[string]struct{}{}}
		for _, id := range doc.IDs {
			cur.completed[id] = struct{}{}
		}
		c.cursors[path] = cur
	}
}

// save atomically persists the cursor state when it has changed since the last
// save, pruning entries whose transcript no longer exists (ended sessions) so
// the file stays bounded by live churn. Errors are non-fatal.
func (c *completionTracker) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" || !c.dirty {
		return nil
	}
	docs := make(map[string]cursorDoc, len(c.cursors))
	for path, cur := range c.cursors {
		if _, err := os.Stat(path); err != nil {
			continue // transcript gone — drop the stale cursor
		}
		ids := make([]string, 0, len(cur.completed))
		for id := range cur.completed {
			ids = append(ids, id)
		}
		docs[path] = cursorDoc{Offset: cur.offset, IDs: ids}
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(docs)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // operator-owned path
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// completed reports whether toolUseID has a tool_result in the transcript at
// path, advancing the cursor over any newly-appended lines first. An empty
// path/id or a missing/unreadable transcript yields false: unknown completion
// is treated as "not yet stopped", the conservative choice that keeps a
// genuinely-live subagent visible rather than silently dropping it.
func (c *completionTracker) completed(path, toolUseID string) bool {
	if path == "" || toolUseID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	cur := c.cursors[path]
	if cur == nil {
		cur = &transcriptCursor{completed: map[string]struct{}{}}
		c.cursors[path] = cur
	}
	c.advance(path, cur)
	_, ok := cur.completed[toolUseID]
	return ok
}

// advance reads lines appended to path since the cursor's offset, folding the
// tool_use_id of every tool_result into the completed set. It advances the
// offset only past whole (newline-terminated) lines, so a partially-written
// final line is re-read on the next tick once complete. A file shorter than the
// stored offset (truncated or rotated) resets the cursor before rescanning.
func (c *completionTracker) advance(path string, cur *transcriptCursor) {
	f, err := os.Open(path) //nolint:gosec // operator-owned transcript path
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	if fi, err := f.Stat(); err == nil && fi.Size() < cur.offset {
		cur.offset = 0
		cur.completed = map[string]struct{}{}
	}
	if _, err := f.Seek(cur.offset, io.SeekStart); err != nil {
		return
	}

	startOffset := cur.offset
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur.offset += int64(len(line))
			scanCompletedIDs(line, cur.completed)
		}
		if err != nil {
			// io.EOF (optionally with a trailing partial line) or a read error:
			// stop. The partial line is deliberately not counted toward offset
			// and is retried once fully written.
			break
		}
	}
	// Any consumed bytes (advanced offset) or new ids are worth persisting so
	// the next run resumes here instead of re-reading from startOffset.
	if cur.offset != startOffset {
		c.dirty = true
	}
}

// scanCompletedIDs folds the tool_use_id of every tool_result carried by one
// transcript line into dst. The substring gate skips the vast majority of lines
// (assistant turns, tool_use requests) that carry no tool_result before the
// comparatively costly JSON parse. A line whose message.content is a bare
// string (not an array) fails the unmarshal and is skipped — such lines never
// carry a tool_result anyway.
func scanCompletedIDs(line []byte, dst map[string]struct{}) {
	if !bytes.Contains(line, []byte(`"tool_result"`)) {
		return
	}
	var rec struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	for _, item := range rec.Message.Content {
		if item.Type == "tool_result" && item.ToolUseID != "" {
			dst[item.ToolUseID] = struct{}{}
		}
	}
}
