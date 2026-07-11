// cmd_compact_freeze.go — arms the post-compaction dispatch freeze.
//
// Subcommand: claude-hook-fast compact-freeze-arm   (PreCompact hook)
//
// Compaction replaces live context with a summary that is authoritative about
// intent but lossy about in-flight state: which agents are already dispatched,
// which worktrees are dirty, which of two similar design docs is the live one.
// Real incident, 2026-07-11: a post-compaction session re-derived its plan
// from a duplicate design doc, concluded the components were unbuilt, and
// dispatched a second agent wave into territories the first wave was actively
// editing — duplicate token spend plus clobbered uncommitted work.
//
// This subcommand writes the "compact_freeze" counter (=1) into the rules
// engine's state dir for the USER-GLOBAL config layer. The companion rules in
// ~/.claude/hook-rules.json read that counter and hard-block the FIRST
// Agent/Task/Workflow dispatch after any compaction with fire_once semantics:
// the block consumes the counter, so exactly one dispatch attempt is stopped
// and redirected to a verification pass (live agent roster + git status of
// every target repo) before fan-out resumes.
//
// State-dir contract: the global config's projectDir is the directory that
// contains .claude/ — for ~/.claude/hook-rules.json that is $HOME — so the
// counter is written to stateDir($HOME, session_id), exactly where
// evalRulesConfig will look for it. Fail-open: any error exits 0 silently.

package main

import (
	"encoding/json"
	"io"
	"os"
)

func cmdCompactFreezeArm() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) == 0 {
		return
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	armCompactFreeze(in.SessionID)
}

// armCompactFreeze writes the freeze counter for one session. Split out for
// testability; sessionID "" falls back to "unknown" (same as cmdRules).
func armCompactFreeze(sessionID string) {
	if sessionID == "" {
		sessionID = "unknown"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	writeCounter(stateDir(home, sessionID), "compact_freeze", 1)
}
