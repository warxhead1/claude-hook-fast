// Package project resolves a working directory to a canonical
// (project, worktree) pair so host hooks emit consistent identity
// across direct checkouts and worktree-style layouts.
//
// The operator's worktree convention is documented in CLAUDE.md
// ("Worktree discipline"):
//
//	Direct checkout:  /home/eric/projects/<project>
//	Bare-repo wt:     /home/eric/projects/<project>/<worktree>
//	                  e.g. /home/eric/projects/hearth-loom/main
//	                       /home/eric/projects/hearth-loom/wt/<branch>
//
// Before this helper, hooks emitted `project: basename(cwd)`, which
// surfaces "main" for everyone using the standard worktree layout —
// uninformative and indistinguishable from genuinely-named directories
// across projects. This helper returns:
//
//	canonical: "hearth-loom"  (the repo / brand identity)
//	worktree:  "main"         (which checkout inside the repo)
//
// For direct checkouts where the cwd basename IS the project, worktree
// is "" — callers render it as just `<canonical>`.
//
// Composite display: `Display(canonical, worktree)` → "hearth-loom [main]"
// for the worktree case, "tengine" for the direct-checkout case.
//
// Mirrors internal/executor/project_path.go::MatchesProjectName so the
// validator and the hook agree on what "project" means.
package project

import (
	"os"
	"path/filepath"
	"strings"
)

// knownProjectsRoot is the prefix beneath which every <project> /
// <project>/<wt> layout lives. Operator convention. Override with
// $HEARTH_HOOK_PROJECTS_ROOT for tests or non-default layouts.
const knownProjectsRoot = "/home/eric/projects"

// Resolve returns the canonical project name and worktree (when one
// applies) for cwd. The lookup walks ancestors until it finds a .git
// marker; the project name is then derived from the layout:
//
//	.../projects/<project>                          → canonical=<project>, worktree=""
//	.../projects/<project>/<wt>                     → canonical=<project>, worktree=<wt>
//	.../projects/<project>/<wt>/sub                 → canonical=<project>, worktree=<wt>
//	.../projects/<project>/.claude/worktrees/<x>    → canonical=<project>, worktree=<x>
//	.../projects/<project>/.claude/worktrees/<x>/y  → canonical=<project>, worktree=<x>/y
//
// When cwd isn't under the conventional projects root, we fall back to
// the .git ancestor's basename for canonical and leave worktree empty
// (the most we can know without operator convention).
//
// Empty inputs return ("unknown", "").
func Resolve(cwd string) (canonical, worktree string) {
	if cwd == "" {
		return "unknown", ""
	}

	root := projectsRoot()

	// Check for the bare-worktree layout first (more specific path).
	if can, wt, ok := fromBareWorktreesLayout(cwd, root); ok {
		return can, wt
	}

	// Then check for the .claude/worktrees/<agent> pattern directly on cwd,
	// because the .git ancestor for those worktrees lives elsewhere (Claude
	// manages the worktree under <project>/.claude/worktrees/), so the
	// generic git-ancestor walk maps them to <project>/.claude instead of
	// the agent leaf. Operator convention is to surface the agent name.
	if can, wt, ok := fromClaudeWorktreesLayout(cwd, root); ok {
		return can, wt
	}

	// First find the nearest .git ancestor — that anchors the worktree.
	wtRoot := findGitAncestor(cwd)
	if wtRoot == "" {
		// No git ancestor reachable; fall back to convention parsing.
		return fromLayout(cwd, root)
	}

	// Map the worktree root to the convention.
	can, wt := fromLayout(wtRoot, root)
	return can, wt
}

// fromClaudeWorktreesLayout detects the canonical Claude-managed
// worktree layout: <root>/<project>/.claude/worktrees/<agent>[/...].
// When matched, worktree = <agent> (or <agent>/<sub>/... if the cwd is
// nested deeper inside the worktree).
//
// Returns (canonical, worktree, true) on a match. Returns ok=false when
// the cwd doesn't contain the `.claude/worktrees/` segment under the
// projects root.
// fromBareWorktreesLayout detects the bare-repo worktree layout used by
// Claude Code agents: <root>/<project>/.bare/.claude/worktrees/<agent>[/...].
// When matched, worktree = <agent> (or <agent>/<sub>/... if nested deeper).
//
// Returns (canonical, worktree, true) on a match. Returns ok=false when
// the cwd doesn't contain the `.bare/.claude/worktrees/` segment under the
// projects root.
func fromBareWorktreesLayout(cwd, root string) (canonical, worktree string, ok bool) {
	clean := filepath.Clean(cwd)
	rootClean := filepath.Clean(root)

	rel, err := filepath.Rel(rootClean, clean)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// Need at least <project>/.bare/.claude/worktrees/<agent>
	if len(parts) < 5 {
		return "", "", false
	}
	if parts[1] != ".bare" || parts[2] != ".claude" || parts[3] != "worktrees" {
		return "", "", false
	}
	can := parts[0]
	wt := strings.Join(parts[4:], string(filepath.Separator))
	return can, wt, true
}

func fromClaudeWorktreesLayout(cwd, root string) (canonical, worktree string, ok bool) {
	clean := filepath.Clean(cwd)
	rootClean := filepath.Clean(root)

	rel, err := filepath.Rel(rootClean, clean)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// Need at least <project>/.claude/worktrees/<agent>
	if len(parts) < 4 {
		return "", "", false
	}
	if parts[1] != ".claude" || parts[2] != "worktrees" {
		return "", "", false
	}
	can := parts[0]
	wt := strings.Join(parts[3:], string(filepath.Separator))
	return can, wt, true
}

// Display renders (canonical, worktree) into the operator-preferred
// "project [worktree]" form. Returns just the canonical name for
// direct checkouts.
func Display(canonical, worktree string) string {
	if worktree == "" || worktree == canonical {
		return canonical
	}
	return canonical + " [" + worktree + "]"
}

func projectsRoot() string {
	if v := os.Getenv("HEARTH_HOOK_PROJECTS_ROOT"); v != "" {
		return filepath.Clean(v)
	}
	return knownProjectsRoot
}

// fromLayout maps a directory (the worktree root, or cwd when no git
// ancestor is found) to (canonical, worktree) using the conventional
// layout under root. Returns ("<basename>", "") for paths outside
// the convention.
func fromLayout(dir, root string) (canonical, worktree string) {
	clean := filepath.Clean(dir)
	rootClean := filepath.Clean(root)

	rel, err := filepath.Rel(rootClean, clean)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		// Outside the conventional root — use basename only.
		base := filepath.Base(clean)
		if base == "" || base == "/" || base == "." {
			return "unknown", ""
		}
		return base, ""
	}

	parts := strings.Split(rel, string(filepath.Separator))
	switch {
	case len(parts) == 0 || parts[0] == "":
		return "unknown", ""
	case len(parts) == 1:
		// .../projects/<project>  — direct checkout
		return parts[0], ""
	default:
		// .../projects/<project>/<worktree>[/...]
		// Worktree is the immediate child; sub-paths get folded into it
		// only when the operator nested further (rare). Keep just one
		// segment for the worktree to stay readable in displays.
		return parts[0], parts[1]
	}
}

// findGitAncestor walks up from start looking for a .git marker (file or
// directory — git worktrees use a regular file pointing at the bare repo).
// Bounded to 12 levels so a pathological cwd never spins forever.
func findGitAncestor(start string) string {
	cur := filepath.Clean(start)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
	return ""
}
