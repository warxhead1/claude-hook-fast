package main

// cmdWorktreeWriteGuard — GLOBAL PreToolUse guard: force worktree isolation
// for file-writing tools, across ALL repos. Go port of
// ~/.claude/hooks/pre-tool-use/worktree-write-guard.py, plus the v2
// ephemeral-root exemption (primaries under /tmp etc. always pass).
//
// Rules, applied to Write|Edit|MultiEdit|NotebookEdit targets that resolve
// inside a PRIMARY checkout (writes to worktrees / non-repos always pass):
//   0. allowlist: .planning/ .claude/ .beads/ CLAUDE.md and *.md pass
//   1. WAVE FREEZE: if ~/.claude/wave-freeze/<repo> marker is fresh, block
//      EVERYONE (lead included).
//   2. SUBAGENT: transcript_path containing '/tasks/' -> block.
//   3. ANCHORING: cwd is inside a worktree but the target is the SAME
//      repo's primary checkout -> block (absolute-path main-anchoring
//      leak). A different repo's primary is a deliberate cross-repo edit:
//      allowed, logged with rule=cross-repo.
//
// Exit 0 = allow; exit 2 = block. Kill switch: WORKTREE_GUARD_DISABLE=1.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

var writeGuardTools = map[string]bool{
	"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true,
}

// writeGuardVerdict is the pure core: "", "freeze", "subagent", or
// "anchoring" for a resolved target path.
func writeGuardVerdict(target, cwd string, isAgent bool) (verdict, root string) {
	r, kind := repoOf(target)
	if kind != "primary" {
		return "", r // worktree or non-repo target: always fine
	}
	if isEphemeral(r) {
		return "", r // scratch clone under /tmp etc.
	}
	if allowlisted(target, r) {
		return "", r
	}
	if freezeActive(r) {
		return "freeze", r
	}
	if isAgent {
		return "subagent", r
	}
	if cwd != "" {
		if wroot, ck := repoOf(cwd); ck == "worktree" {
			// The main-anchoring leak this rule exists for is a worktree
			// session writing into ITS OWN repo's primary checkout via an
			// absolute path. A write into a DIFFERENT repo's primary is a
			// deliberate cross-repo edit (ops files, sister-repo configs) —
			// blocking it only trains Bash-level bypasses (measured
			// 2026-08-28: two operator-approved compose edits detoured
			// through python heredocs). Allow-with-log so audits still see
			// it. If the worktree's primary can't be resolved, keep the
			// block: fail closed to the original behavior.
			if pr, ok := worktreePrimary(wroot); ok && pr != r {
				return "cross-repo", r
			}
			return "anchoring", r
		}
	}
	return "", r
}

func cmdWorktreeWriteGuard() {
	if wgDisabled() {
		return
	}
	input, ok := hookio.ReadHookInput()
	if !ok || !writeGuardTools[input.ToolName] {
		return
	}
	var ti struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
	}
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
		return
	}
	path := ti.FilePath
	if path == "" {
		path = ti.NotebookPath
	}
	if path == "" {
		path = ti.Path
	}
	if path == "" {
		return
	}

	target := realpathBest(path)
	isAgent := hookio.IsSubagentTranscript(input.TranscriptPath)
	verdict, root := writeGuardVerdict(target, input.Cwd, isAgent)
	if verdict == "cross-repo" {
		wgLog("write-guard", "ALLOW", fmt.Sprintf(
			"tool=%s target=%s repo=%s agent=%v tx=%s rule=cross-repo cwd=%s",
			input.ToolName, target, root, isAgent, input.TranscriptPath, input.Cwd))
		return
	}
	// tx in the detail is forensic: isAgent now catches both the /tasks/
	// (fresh) and /subagents/ (resumed) transcript forms, but if a future
	// harness path shape slips past IsSubagentTranscript, the logged tx is
	// what proves which session context actually made a primary-tree write.
	detail := fmt.Sprintf("tool=%s target=%s repo=%s agent=%v tx=%s",
		input.ToolName, target, root, isAgent, input.TranscriptPath)
	if verdict == "" {
		if root != "" {
			wgLog("write-guard", "ALLOW", detail)
		}
		return
	}

	var lines []string
	switch verdict {
	case "freeze":
		lines = []string{
			fmt.Sprintf("  Tool:   %s", input.ToolName),
			fmt.Sprintf("  Path:   %s", target),
			fmt.Sprintf("  Repo:   %s  (WAVE FREEZE active)", root),
			"",
			"A dispatch wave is live for this repo: agents may be running whose",
			"isolation the harness silently dropped, so ALL writes to the shared",
			"checkout are frozen (allowlisted: .planning/ .claude/ .beads/ *.md).",
			"",
			"  Agents:  do your work in YOUR worktree (bare-relative paths from",
			"           $PWD). If you have no worktree, STOP and escalate to the",
			"           lead — do not retry against the shared tree.",
			"  Lead:    integrate after agents quiesce, then lift the freeze:",
			fmt.Sprintf("             wave-freeze off %s", root),
			"  Cross-project task with no workspace here? The lead mints one:",
			fmt.Sprintf("             xwt new %s", filepath.Base(root)),
		}
		detail += " rule=freeze"
	case "subagent":
		lines = []string{
			fmt.Sprintf("  Tool:   %s", input.ToolName),
			fmt.Sprintf("  Path:   %s", target),
			fmt.Sprintf("  Repo:   %s  (shared primary checkout)", root),
			"",
			"You are a dispatched subagent. Subagents NEVER write the shared",
			"checkout of ANY repo — that is how sibling work gets silently",
			"destroyed. Work inside your isolated worktree using bare-relative",
			"paths from $PWD.",
			"",
			"If you were not given a worktree, STOP and report to the dispatcher",
			"rather than retrying — your dispatch is wrong, not this guard.",
			"Cross-project task (this repo is not the dispatching session's)?",
			fmt.Sprintf("The lead mints your workspace: `xwt new %s`.", filepath.Base(root)),
		}
		detail += " rule=subagent"
	case "anchoring":
		lines = []string{
			fmt.Sprintf("  Tool:   %s", input.ToolName),
			fmt.Sprintf("  Path:   %s", target),
			fmt.Sprintf("  Repo:   %s  (shared primary checkout)", root),
			fmt.Sprintf("  Cwd:    %s  (a worktree)", input.Cwd),
			"",
			"Your working directory is an isolated worktree, but this write",
			"resolves into a SHARED primary checkout — a main-anchoring leak",
			"from an absolute path in your prompt or context.",
			"",
			"Fix: use bare-relative paths (crates/foo/bar.rs) or $PWD-prefixed",
			"paths so the write lands in YOUR worktree.",
		}
		detail += fmt.Sprintf(" rule=anchoring cwd=%s", input.Cwd)
	}

	wgLog("write-guard", "BLOCK", detail)
	fmt.Fprint(os.Stderr, "WORKTREE WRITE GUARD (global): blocked write to a shared "+
		"primary checkout.\n\n")
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	hookExit(2)
}
