// Live-filesystem Reader for claude-steward.
//
// # Purpose
//
// fileReader implements steward.Reader: it assembles one tick's Snapshot by
// reading the runtime-signal files this machine's Claude Code install
// actually writes — the statusline's usage cache, one JSON file per live
// session, the statusline's per-session context/pane file, the daemon
// roster, and lingering-subagent transcript files. Nothing here decides
// anything; the pure steward.Decide core in internal/steward consumes the
// Snapshot this produces.
//
// # Design constraints
//
//   - This is the ONLY I/O edge in this file; internal/steward stays pure.
//   - Every source is read defensively: a missing, truncated, or malformed
//     file degrades that one field/session to its zero value (Present:false,
//     skip the session, skip the subagent) — it never aborts the whole read
//     and never panics. The daemon's fire-and-forget discipline requires a
//     tick to always produce SOME snapshot.
//   - All paths are overridable via readerPaths so tests point the reader at
//     a t.TempDir() fixture tree instead of the real $HOME/$XDG_RUNTIME_DIR.
//   - snap.BurnPctPerMin is deliberately left 0 here — the daemon's main loop
//     computes the EMA across successive ticks, which needs history this
//     single-tick Reader does not have.
//
// # Failure modes handled
//
//   - Usage cache absent/malformed → both windows Present:false (observe-only
//     degrade, per the daemon's contract with steward.Reader).
//   - Session file for a pid that is no longer running → session skipped
//     entirely (stale file left behind by a crashed/killed process).
//   - Context file present but is_subagent:true → belongs to a subagent's own
//     tracking, not the parent orchestrator; skipped so it can never steal
//     the parent session's pane binding.
//   - Roster file absent/malformed → no session gets Kind=KindBackground; all
//     sessions fall back to KindInteractive with no PTY path.
//   - Subagent transcript glob errors or unreadable files → that subagent is
//     skipped, not fatal to the session it belongs to.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

// readerPaths holds every absolute path/dir the fileReader consults, all
// overridable so tests can redirect the whole tree into a t.TempDir() without
// touching the real $HOME or $XDG_RUNTIME_DIR. Any field left zero is defaulted
// by newFileReader from the environment.
type readerPaths struct {
	// UsageCache is the statusline's cached OAuth usage payload.
	// Default: /tmp/claude-usage-cache.json
	UsageCache string
	// SessionsDir holds one <pid>.json per live/former Claude Code process.
	// Default: $HOME/.claude/sessions
	SessionsDir string
	// ContextRuntimeDir holds claude-context-<sessionId>.json files, the
	// statusline's primary (XDG runtime dir) location.
	// Default: ${XDG_RUNTIME_DIR:-/run/user/1000}/hearth/context
	ContextRuntimeDir string
	// ContextFallbackDir holds claude-context-cli_<sessionId>.json files, the
	// statusline's fallback location when no XDG runtime dir is available.
	// Default: /tmp
	ContextFallbackDir string
	// RosterFile is the daemon's background-worker roster.
	// Default: $HOME/.claude/daemon/roster.json
	RosterFile string
	// ProjectsDir is the root under which per-project, per-session subagent
	// transcripts live: <ProjectsDir>/<project-slug>/<sessionId>/subagents/.
	// Default: $HOME/.claude/projects
	ProjectsDir string
	// IgnoreDir holds one empty marker file per opted-out session ID.
	// Default: $HOME/.claude/steward/ignore
	IgnoreDir string
	// LastCompactDir optionally holds one file per session ID containing a
	// unix-seconds int: the last time the steward compacted it.
	// Default: $HOME/.claude/steward/last-compact
	LastCompactDir string
	// CompletionCache is the persisted subagent-completion cursor state (see
	// completion.go). Empty disables persistence (in-memory only).
	// Default: $HOME/.claude/steward/completions.json
	CompletionCache string
}

// fileReader is the concrete steward.Reader that reads all of the above from
// disk on every Read call. Its only cross-tick state is the completion tracker
// (see completion.go): a pure, disk-rebuildable cache of which subagents have
// finished, so a completed subagent's lingering transcript is not mistaken for
// a still-running one. Every other field is derived fresh each tick.
type fileReader struct {
	paths      readerPaths
	completion *completionTracker
}

// newFileReader returns a fileReader with any zero-valued path in p defaulted
// from $HOME / $XDG_RUNTIME_DIR, so production wiring can pass a mostly-empty
// readerPaths{} and tests can override exactly the paths they need to control.
func newFileReader(p readerPaths) *fileReader {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = os.Getenv("HOME")
	}
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntime == "" {
		xdgRuntime = "/run/user/1000"
	}

	if p.UsageCache == "" {
		p.UsageCache = "/tmp/claude-usage-cache.json"
	}
	if p.SessionsDir == "" {
		p.SessionsDir = filepath.Join(home, ".claude", "sessions")
	}
	if p.ContextRuntimeDir == "" {
		p.ContextRuntimeDir = filepath.Join(xdgRuntime, "hearth", "context")
	}
	if p.ContextFallbackDir == "" {
		p.ContextFallbackDir = "/tmp"
	}
	if p.RosterFile == "" {
		p.RosterFile = filepath.Join(home, ".claude", "daemon", "roster.json")
	}
	if p.ProjectsDir == "" {
		p.ProjectsDir = filepath.Join(home, ".claude", "projects")
	}
	if p.IgnoreDir == "" {
		p.IgnoreDir = filepath.Join(home, ".claude", "steward", "ignore")
	}
	if p.LastCompactDir == "" {
		p.LastCompactDir = filepath.Join(home, ".claude", "steward", "last-compact")
	}
	if p.CompletionCache == "" {
		p.CompletionCache = filepath.Join(home, ".claude", "steward", "completions.json")
	}
	return &fileReader{paths: p, completion: newCompletionTracker(p.CompletionCache)}
}

// Read implements steward.Reader. It never returns a partial-but-fatal error
// for missing optional sources (usage cache, roster, context files) — those
// degrade fields to zero values. The only error path is ctx already being
// canceled when the tick starts.
func (r *fileReader) Read(ctx context.Context, now time.Time, prev steward.Level, cfg steward.Config) (steward.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return steward.Snapshot{}, err
	}

	snap := steward.Snapshot{
		Now:       now,
		PrevLevel: prev,
		Config:    cfg,
	}

	snap.FiveHour, snap.SevenDay = rdReadUsage(r.paths.UsageCache)

	roster := rdReadRoster(r.paths.RosterFile) // sessionID -> worker; empty map on any failure

	for _, sf := range rdReadSessions(r.paths.SessionsDir) {
		if !rdPidAlive(sf.Pid) {
			continue // stale session file left behind by a dead process
		}

		sess := steward.SessionState{
			ID:     sf.SessionID,
			Name:   sf.Name,
			Kind:   steward.KindInteractive,
			Status: sf.Status,
		}

		if sess.Status == "idle" && sf.StatusUpdatedAt > 0 {
			statusAt := time.UnixMilli(sf.StatusUpdatedAt)
			sess.IdleFor = rdNonNegative(now.Sub(statusAt))
			// CacheAge approximates the prompt-cache age as the time since the
			// last API call, which we take to be when the turn ended (i.e. the
			// same statusUpdatedAt timestamp). This is an approximation pending
			// a proper read of the transcript's last-message timestamp.
			sess.CacheAge = sess.IdleFor
		}

		if ctxFile, ok := rdReadContext(r.paths, sess.ID); ok {
			sess.ContextPct = ctxFile.CurrentContext
			sess.PaneID = rdPaneID(ctxFile)
		}

		if w, ok := roster[sess.ID]; ok {
			sess.Kind = steward.KindBackground
			sess.PtySock = w.PtySock
			sess.PtyAuthAvail = w.PtyAuth != "" && rdPathExists(w.PtySock)
		}

		sess.Ignored = rdPathExists(filepath.Join(r.paths.IgnoreDir, sess.ID))
		sess.LastCompact = rdReadLastCompact(r.paths.LastCompactDir, sess.ID)

		subs := rdFindSubagents(r.paths.ProjectsDir, sess.ID, now, r.completion)
		for _, sub := range subs {
			// A completed subagent is never "active" no matter how its
			// transcript's mtime looks; only a not-yet-stopped, recently-active
			// child counts toward the compaction-blocking active tally.
			if !sub.Stopped && sub.StaleFor < cfg.LingeringSubagentAfter {
				sess.SubagentsActive++
			}
		}
		snap.Subagents = append(snap.Subagents, subs...)

		snap.Sessions = append(snap.Sessions, sess)
	}

	// Persist any advanced transcript cursors so the next tick (or a restart /
	// --once run) resumes incrementally instead of re-scanning from the top. A
	// failed save is non-fatal — it only costs one cold rescan next time.
	_ = r.completion.save()

	return snap, nil
}

// rdNonNegative clamps a duration to zero; a clock skew or out-of-order
// timestamp must never surface as a negative IdleFor/CacheAge.
func rdNonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// rdPathExists reports whether path can be stat'd, treating any error
// (including permission-denied) as "does not exist" — never fatal.
func rdPathExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// rdPidAlive reports whether pid names a live process, using signal 0 (no
// actual signal delivered) per the standard liveness-check idiom. ESRCH means
// dead; EPERM (exists, owned by another user) still counts as alive.
func rdPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// --- usage cache -----------------------------------------------------------

// rdWindowRaw mirrors one rate-limit window in the usage cache payload,
// accepting either field name the endpoint has been observed to use.
type rdWindowRaw struct {
	Utilization    *float64 `json:"utilization"`
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       string   `json:"resets_at"`
}

// rdUsagePayload mirrors the statusline's cache file, tolerating both a
// top-level five_hour/seven_day pair and the same pair nested under "usage".
type rdUsagePayload struct {
	FiveHour *rdWindowRaw `json:"five_hour"`
	SevenDay *rdWindowRaw `json:"seven_day"`
	Usage    *struct {
		FiveHour *rdWindowRaw `json:"five_hour"`
		SevenDay *rdWindowRaw `json:"seven_day"`
	} `json:"usage"`
}

// rdReadUsage reads the usage cache, returning both windows with
// Present:false (not an error) on any missing/malformed file.
func rdReadUsage(path string) (fiveHour, sevenDay steward.Window) {
	data, err := os.ReadFile(path)
	if err != nil {
		return steward.Window{}, steward.Window{}
	}
	var payload rdUsagePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return steward.Window{}, steward.Window{}
	}
	fh, sd := payload.FiveHour, payload.SevenDay
	if payload.Usage != nil {
		fh, sd = payload.Usage.FiveHour, payload.Usage.SevenDay
	}
	return rdParseWindow(fh), rdParseWindow(sd)
}

func rdParseWindow(raw *rdWindowRaw) steward.Window {
	if raw == nil {
		return steward.Window{Present: false}
	}
	w := steward.Window{Present: true}
	switch {
	case raw.Utilization != nil:
		w.Utilization = *raw.Utilization
	case raw.UsedPercentage != nil:
		w.Utilization = *raw.UsedPercentage
	}
	if raw.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, raw.ResetsAt); err == nil {
			w.ResetsAt = t
		}
	}
	return w
}

// --- sessions ----------------------------------------------------------------

// rdSessionFile mirrors ~/.claude/sessions/<pid>.json.
type rdSessionFile struct {
	Pid             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StartedAt       int64  `json:"startedAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
	Version         string `json:"version"`
	Kind            string `json:"kind"`
}

// rdReadSessions globs dir for *.json and returns every file that parses.
// Malformed individual files are skipped, not fatal to the whole directory.
func rdReadSessions(dir string) []rdSessionFile {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil
	}
	var out []rdSessionFile
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var sf rdSessionFile
		if err := json.Unmarshal(data, &sf); err != nil {
			continue
		}
		if sf.SessionID == "" {
			continue
		}
		out = append(out, sf)
	}
	return out
}

// --- context / pane ----------------------------------------------------------

// rdContextFile mirrors claude-context-<sessionId>.json / the _cli_ fallback.
type rdContextFile struct {
	CurrentContext int     `json:"currentContext"`
	TmuxPane       *string `json:"tmux_pane"`
	ZellijPaneID   *int64  `json:"zellij_pane_id"`
	Pid            int     `json:"pid"`
	IsSubagent     bool    `json:"is_subagent"`
}

// rdReadContext locates the context file for sessionID (primary XDG runtime
// location, then the /tmp fallback) and returns it. ok is false if neither
// file exists/parses, OR if the file belongs to a subagent (is_subagent:true)
// — such a file is never allowed to represent the parent orchestrator
// session, per the steward's pane-binding safety contract.
func rdReadContext(p readerPaths, sessionID string) (rdContextFile, bool) {
	candidates := []string{
		filepath.Join(p.ContextRuntimeDir, "claude-context-"+sessionID+".json"),
		filepath.Join(p.ContextFallbackDir, "claude-context-cli_"+sessionID+".json"),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cf rdContextFile
		if err := json.Unmarshal(data, &cf); err != nil {
			continue
		}
		if cf.IsSubagent {
			return rdContextFile{}, false
		}
		return cf, true
	}
	return rdContextFile{}, false
}

// rdPaneID renders the resolved pane token: "tmux:<pane>" takes precedence
// over "zellij:<id>"; "" if neither is set.
func rdPaneID(cf rdContextFile) string {
	if cf.TmuxPane != nil && *cf.TmuxPane != "" {
		return "tmux:" + *cf.TmuxPane
	}
	if cf.ZellijPaneID != nil {
		return "zellij:" + strconv.FormatInt(*cf.ZellijPaneID, 10)
	}
	return ""
}

// --- daemon roster -------------------------------------------------------------

// rdRosterWorker mirrors one entry of roster.json's "workers" map.
type rdRosterWorker struct {
	SessionID string `json:"sessionId"`
	Pid       int    `json:"pid"`
	PtySock   string `json:"ptySock"`
	PtyAuth   string `json:"ptyAuth"`
	Cwd       string `json:"cwd"`
}

type rdRosterFile struct {
	Workers map[string]rdRosterWorker `json:"workers"`
}

// rdReadRoster reads roster.json and reindexes it by sessionId (the roster's
// own key is a short worker name, not the session ID sessions carry). Returns
// an empty, non-nil map on any missing/malformed file.
//
// Security note: PtyAuth is carried only transiently through this function's
// local map and into SessionState.PtyAuthAvail's boolean computation — it is
// never copied onto SessionState or any other logged/persisted structure.
func rdReadRoster(path string) map[string]rdRosterWorker {
	out := map[string]rdRosterWorker{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var rf rdRosterFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return out
	}
	for _, w := range rf.Workers {
		if w.SessionID == "" {
			continue
		}
		out[w.SessionID] = w
	}
	return out
}

// --- lingering subagents -------------------------------------------------------

// rdSubagentMeta mirrors the fields of a subagent's sibling <id>.meta.json that
// the steward consumes: the display name and the toolUseId that ties a
// dispatched Task/Agent subagent to its completing tool_result in the parent
// transcript. In-process teammates carry no toolUseId.
type rdSubagentMeta struct {
	Name      string `json:"name"`
	ToolUseID string `json:"toolUseId"`
}

// rdFindSubagents globs <ProjectsDir>/*/<sessionID>/subagents/agent-*.jsonl
// (the glob avoids recomputing the cwd->slug transform) and builds one
// steward.SubagentState per transcript file found.
//
// Started is true for any transcript on disk. Stopped is resolved from the
// authoritative completion signal: a dispatched subagent's meta.json carries a
// toolUseId, and its completion is recorded in the PARENT session transcript
// (<slug>/<sessionID>.jsonl) as a tool_result bearing that id. The tracker
// scans that transcript incrementally. A subagent with no toolUseId (an
// in-process teammate) or one whose completion the parent transcript does not
// yet confirm stays Stopped:false, so mtime staleness alone decides whether it
// is lingering — the prior behavior, now the fallback rather than the rule.
func rdFindSubagents(projectsDir, sessionID string, now time.Time, tracker *completionTracker) []steward.SubagentState {
	pattern := filepath.Join(projectsDir, "*", sessionID, "subagents", "agent-*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	var out []steward.SubagentState
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		base := filepath.Base(m)
		id := strings.TrimSuffix(base, ".jsonl")

		name, toolUseID := rdReadSubagentMeta(m, id)

		stopped := false
		if toolUseID != "" && tracker != nil {
			// The subagents/ dir sits directly under the parent session dir;
			// the parent transcript is that dir plus ".jsonl".
			sessionDir := filepath.Dir(filepath.Dir(m))
			stopped = tracker.completed(sessionDir+".jsonl", toolUseID)
		}

		out = append(out, steward.SubagentState{
			ID:       id,
			ParentID: sessionID,
			Name:     name,
			Started:  true,
			Stopped:  stopped,
			StaleFor: rdNonNegative(now.Sub(info.ModTime())),
		})
	}
	return out
}

// rdReadSubagentMeta reads the sibling <id>.meta.json once, returning the
// display name (falling back to the transcript-derived id, stripping the
// "agent-" prefix, when no meta is present or it fails to parse) and the
// toolUseId that ties this subagent to a tool_result in its parent transcript
// ("" for teammates and other classes that carry no dispatch tool_use).
func rdReadSubagentMeta(transcriptPath, id string) (name, toolUseID string) {
	name = strings.TrimPrefix(id, "agent-")
	metaPath := strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return name, ""
	}
	var meta rdSubagentMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return name, ""
	}
	if meta.Name != "" {
		name = meta.Name
	}
	return name, meta.ToolUseID
}

// --- opt-out / compaction history ----------------------------------------------

// rdReadLastCompact reads dir/<sessionID> as a unix-seconds int and returns
// the corresponding time, or the zero Time if the file is absent/malformed.
func rdReadLastCompact(dir, sessionID string) time.Time {
	data, err := os.ReadFile(filepath.Join(dir, sessionID))
	if err != nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}
