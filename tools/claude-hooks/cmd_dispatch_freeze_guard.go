package main

// cmdDispatchFreezeGuard — GLOBAL PreToolUse guard for Agent|Task|Workflow
// dispatch. Go port of ~/.claude/hooks/pre-tool-use/dispatch-freeze-guard.py.
//
// Three jobs, for worktree-isolated dispatches only:
//
//  1. AUTO-FREEZE: drop a wave-freeze marker for every repo the wave touches
//     (the cwd's repo plus any primary checkout referenced through a
//     sanctioned worktree path). While fresh (8h TTL), worktree-write-guard
//     and worktree-bash-guard block all writes to those shared checkouts.
//     Lift after integration: `wave-freeze off <repo-root>`.
//
//  2. BARE-RELATIVE-PATH RULE: a dispatch whose brief contains an absolute
//     path to a CODE file inside the dispatching repo's primary checkout is
//     DENIED — agents anchor on those paths and write to main even when cwd
//     is the worktree. .md and .planning/.claude/.beads references pass.
//
//  3. CROSS-PROJECT AUTO-MINT: a brief referencing a DIFFERENT repo's shared
//     primary checkout gets a sanctioned workspace minted automatically
//     (`xwt new`), the brief's paths rewritten onto the minted worktree, and
//     the dispatch allowed via hookSpecificOutput.updatedInput. Only if the
//     mint fails does the guard block.
//
// Exit 0 = allow; exit 2 = block. Kill switch: WORKTREE_GUARD_DISABLE=1.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

var (
	// ANY absolute path — structural .git-walking decides what it is.
	absPathRe    = regexp.MustCompile(`(/[A-Za-z0-9._][A-Za-z0-9._/-]*)`)
	isolationRe  = regexp.MustCompile(`isolation\s*:\s*['"]worktree['"]`)
	codeExtsList = []string{
		".rs", ".c", ".h", ".cpp", ".hpp", ".cc", ".glsl", ".slang",
		".comp", ".frag", ".vert", ".py", ".ts", ".js", ".go", ".toml",
		".json", ".yaml", ".yml", ".sh", ".tsdl", ".sql", ".kt", ".svelte",
	}
)

func hasCodeExt(p string) bool {
	for _, e := range codeExtsList {
		if strings.HasSuffix(p, e) {
			return true
		}
	}
	return false
}

// dispatchPrimaryRootOf: primary checkout root for a path, resolving
// worktrees via their gitdir pointer. ok=false when not in a repo (or the
// pointer can't be followed).
func dispatchPrimaryRootOf(path string) (string, bool) {
	root, kind := repoOf(path)
	switch kind {
	case "primary":
		return root, true
	case "worktree":
		return worktreePrimary(root)
	}
	return "", false
}

func setFreeze(root, sessionID, tool string) bool {
	if err := os.MkdirAll(wgFreezeDir(), 0o755); err != nil {
		return false
	}
	payload, _ := json.Marshal(map[string]string{
		"repo": root, "set_by": "dispatch-freeze-guard", "tool": tool,
		"session_id": sessionID, "at": time.Now().Format("2006-01-02T15:04:05"),
	})
	return os.WriteFile(freezeMarkerPath(root), payload, 0o644) == nil
}

// mintWorktree mints a sanctioned worktree in a foreign repo via `xwt new`
// (which also wave-freezes the target). Overridable for tests.
var mintWorktree = func(root string) string {
	name := fmt.Sprintf("agent-%s-p%d", time.Now().Format("20060102-150405"), os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xwt", "new", root, name, "--no-bootstrap").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// replaceRootRefs rewrites primary-checkout references onto the minted
// worktree. Skips paths already inside a sanctioned worktree, and requires
// a path or text boundary after the root so sibling repos sharing a name
// prefix (kb vs kb-tools) are never partially rewritten. (Manual scan: RE2
// has no lookahead.)
func replaceRootRefs(text, root, wt string) string {
	const boundary = " \t\n\"'`.,;:)]}"
	var b strings.Builder
	i := 0
	for {
		k := strings.Index(text[i:], root)
		if k < 0 {
			b.WriteString(text[i:])
			break
		}
		k += i
		end := k + len(root)
		match := false
		if end >= len(text) {
			match = true
		} else if text[end] == '/' {
			rest := text[end:]
			if !strings.HasPrefix(rest, "/.claude/worktrees/") &&
				!strings.HasPrefix(rest, "/.worktrees/") {
				match = true
			}
		} else if strings.ContainsRune(boundary, rune(text[end])) {
			match = true
		}
		b.WriteString(text[i:k])
		if match {
			b.WriteString(wt)
		} else {
			b.WriteString(root)
		}
		i = end
	}
	return b.String()
}

func cmdDispatchFreezeGuard() {
	if wgDisabled() {
		return
	}
	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}
	if input.ToolName != "Agent" && input.ToolName != "Task" && input.ToolName != "Workflow" {
		return
	}
	var ti map[string]any
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil || ti == nil {
		return
	}
	prompt, _ := ti["prompt"].(string)
	script, _ := ti["script"].(string)
	text := prompt
	if text == "" {
		text = script
	}

	isolated := ti["isolation"] == "worktree"
	if input.ToolName == "Workflow" && isolationRe.MatchString(script) {
		isolated = true
	}
	if !isolated {
		return
	}

	// --- Rule 2 scan: classify every absolute path in the brief ---
	cwdPrimary := ""
	if input.Cwd != "" {
		cwdPrimary, _ = dispatchPrimaryRootOf(input.Cwd)
	}
	var sameRepo []string
	foreignRoots := map[string]bool{}
	worktreeTargets := map[string]bool{}
	for _, m := range absPathRe.FindAllString(text, -1) {
		p := strings.TrimRight(m, ".,;")
		rp := realpathBest(p)
		probe := rp
		if fi, err := os.Stat(rp); err != nil || !fi.IsDir() {
			probe = filepath.Dir(rp)
		}
		root, kind := repoOf(probe)
		if kind == "" {
			continue // not a repo: fine
		}
		if kind == "worktree" {
			if pr, ok := worktreePrimary(root); ok {
				worktreeTargets[pr] = true // the sanctioned pattern — freeze it
			}
			continue
		}
		rel, err := filepath.Rel(root, rp)
		if err != nil {
			continue
		}
		if strings.HasSuffix(p, ".md") ||
			strings.HasPrefix(rel, ".planning/") ||
			strings.HasPrefix(rel, ".claude/") ||
			strings.HasPrefix(rel, ".beads/") {
			continue
		}
		if cwdPrimary != "" && root != cwdPrimary {
			foreignRoots[root] = true
		} else if hasCodeExt(p) {
			sameRepo = append(sameRepo, p)
		}
	}
	if len(sameRepo) > 0 {
		uniq := map[string]bool{}
		for _, p := range sameRepo {
			uniq[p] = true
		}
		offenders := make([]string, 0, len(uniq))
		for p := range uniq {
			offenders = append(offenders, p)
		}
		sort.Strings(offenders)
		if len(offenders) > 10 {
			offenders = offenders[:10]
		}
		wgLog("dispatch-guard", "BLOCK",
			fmt.Sprintf("abs-paths tool=%s offenders=%v", input.ToolName, offenders))
		fmt.Fprint(os.Stderr,
			"DISPATCH GUARD (global): worktree-isolated dispatch brief contains\n"+
				"absolute paths into a shared primary checkout:\n\n")
		for _, p := range offenders {
			fmt.Fprintf(os.Stderr, "  %s\n", p)
		}
		fmt.Fprint(os.Stderr,
			"\nAgents anchor on absolute paths and write to the shared tree even\n"+
				"when their cwd is a worktree (verified failure mode, memory\n"+
				"feedback_agent_absolute_path_main_anchoring). Rewrite the brief with\n"+
				"BARE-RELATIVE paths (crates/foo/bar.rs) — the worktree has the same\n"+
				"layout. References to .planning/ docs and .md files are allowed.\n")
		os.Exit(2)
	}

	// --- Rule 3: cross-project auto-mint ---
	newText := text
	minted := map[string]string{}
	if len(foreignRoots) > 0 {
		roots := make([]string, 0, len(foreignRoots))
		for r := range foreignRoots {
			roots = append(roots, r)
		}
		// Longest root first so nested/prefix-sharing repo paths never
		// partially rewrite each other.
		sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
		for _, root := range roots {
			wt := mintWorktree(root)
			if wt == "" {
				wgLog("dispatch-guard", "BLOCK",
					fmt.Sprintf("cross-project mint-failed tool=%s root=%s", input.ToolName, root))
				fmt.Fprintf(os.Stderr,
					"DISPATCH GUARD (global): brief targets the shared checkout\n"+
						"of foreign repo %s and auto-minting a worktree there\n"+
						"failed. Mint manually and brief the printed path as the\n"+
						"agent's working root:  xwt new %s\n",
					root, filepath.Base(root))
				os.Exit(2)
			}
			minted[root] = wt
		}
		for _, root := range roots {
			newText = replaceRootRefs(newText, root, minted[root])
		}
		mintedRoots := make([]string, 0, len(minted))
		for r := range minted {
			mintedRoots = append(mintedRoots, r)
		}
		sort.Strings(mintedRoots)
		for _, root := range mintedRoots {
			newText += fmt.Sprintf(
				"\n\n[dispatch-guard] Auto-minted workspace for %s: %s\n"+
					"Work ONLY under that path (the repo's primary checkout is\n"+
					"wave-frozen). If scripts/bootstrap_worktree.sh exists there,\n"+
					"run it once before building.", filepath.Base(root), minted[root])
			worktreeTargets[root] = true
		}
		wgLog("dispatch-guard", "AUTOMINT",
			fmt.Sprintf("tool=%s %v", input.ToolName, minted))
	}

	// --- Rule 1: auto-freeze every repo this wave touches ---
	roots := map[string]bool{}
	for r := range worktreeTargets {
		roots[r] = true
	}
	if cwdPrimary != "" {
		roots[cwdPrimary] = true
	}
	var frozen []string
	for r := range roots {
		if setFreeze(r, input.SessionID, input.ToolName) {
			frozen = append(frozen, r)
		}
	}
	if len(frozen) > 0 {
		sort.Strings(frozen)
		wgLog("dispatch-guard", "FREEZE",
			fmt.Sprintf("tool=%s repos=%v", input.ToolName, frozen))
	}

	if len(minted) > 0 {
		// Hand the rewritten brief back to the harness (replaces the whole
		// tool_input). Exit 0 + JSON on stdout is the updatedInput contract.
		field := "prompt"
		if prompt == "" {
			field = "script"
		}
		ti[field] = newText
		out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse",
			"updatedInput":  ti,
		}})
		if err == nil {
			fmt.Println(string(out))
		}
	}
}
