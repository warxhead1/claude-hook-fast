package project

import (
	"os"
	"path/filepath"
	"testing"
)

// makeFakeProjects builds a tempdir mirroring the operator layout:
//
//	<root>/hearth-loom/main/.git/
//	<root>/hearth-loom/wt/.gitkeep   (wt subdir, no .git marker here)
//	<root>/hearth-loom/wt/loom-x/.git/
//	<root>/tengine/.git/             (direct checkout, no worktree level)
//	<root>/orphan/no-git/            (no .git marker anywhere on the path)
func makeFakeProjects(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	mustGit := func(dir string) {
		must(filepath.Join(dir, ".git"))
	}

	mustGit(filepath.Join(root, "hearth-loom", "main"))
	mustGit(filepath.Join(root, "hearth-loom", "wt", "loom-x"))
	mustGit(filepath.Join(root, "tengine"))
	must(filepath.Join(root, "orphan", "no-git"))
	return root
}

func TestResolve_WorktreeLayout(t *testing.T) {
	root := makeFakeProjects(t)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	can, wt := Resolve(filepath.Join(root, "hearth-loom", "main"))
	if can != "hearth-loom" || wt != "main" {
		t.Errorf("got (%q, %q), want (hearth-loom, main)", can, wt)
	}

	// cwd deeper than worktree root → resolve to worktree root.
	deep := filepath.Join(root, "hearth-loom", "main", "internal", "hooks")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	can, wt = Resolve(deep)
	if can != "hearth-loom" || wt != "main" {
		t.Errorf("deep: got (%q, %q), want (hearth-loom, main)", can, wt)
	}
}

func TestResolve_DirectCheckout(t *testing.T) {
	root := makeFakeProjects(t)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	can, wt := Resolve(filepath.Join(root, "tengine"))
	if can != "tengine" || wt != "" {
		t.Errorf("got (%q, %q), want (tengine, \"\")", can, wt)
	}
}

func TestResolve_OutsideConvention(t *testing.T) {
	root := makeFakeProjects(t)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	// Path with no .git ancestor but UNDER the projects root: we treat
	// the first two path segments as (project, worktree) by convention
	// even when no .git exists yet (operator may have just `mkdir`d
	// the worktree dir prior to clone). This mirrors what
	// MatchesProjectName in internal/executor allows.
	can, wt := Resolve(filepath.Join(root, "orphan", "no-git"))
	if can != "orphan" || wt != "no-git" {
		t.Errorf("got (%q, %q), want (orphan, no-git)", can, wt)
	}

	// Path entirely outside the projects root → basename of cwd,
	// no worktree.
	other := t.TempDir()
	can, wt = Resolve(other)
	if wt != "" {
		t.Errorf("got worktree=%q, want empty for non-convention path", wt)
	}
	if can != filepath.Base(other) {
		t.Errorf("got canonical=%q, want basename %q", can, filepath.Base(other))
	}
}

func TestResolve_Empty(t *testing.T) {
	if can, wt := Resolve(""); can != "unknown" || wt != "" {
		t.Errorf("got (%q, %q)", can, wt)
	}
}

// TestResolve_BareWorktreesLayout pins the bare-repo worktree layout used
// by Claude Code agents: <project>/.bare/.claude/worktrees/<agent>.
// These are created when Claude Code creates worktrees from a bare clone
// and are currently resolving to worktree=".bare" instead of the agent ID.
// Repro: 6 Agent-tool subagents running with
// cwd=/home/eric/projects/hearth-loom/.bare/.claude/worktrees/agent-<X>
// but emitting worktree=".bare".
func TestResolve_BareWorktreesLayout(t *testing.T) {
	root := makeFakeProjects(t)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	// Build the bare-worktree layout under hearth-loom.
	agentDir := filepath.Join(root, "hearth-loom", ".bare", ".claude", "worktrees", "agent-aa9752676264a68dd")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	can, wt := Resolve(agentDir)
	if can != "hearth-loom" || wt != "agent-aa9752676264a68dd" {
		t.Errorf("got (%q, %q), want (hearth-loom, agent-aa9752676264a68dd)", can, wt)
	}

	// Verify Display renders correctly — not [.bare], not [.bare] with
	// the bare segment visible.
	display := Display(can, wt)
	if display != "hearth-loom [agent-aa9752676264a68dd]" {
		t.Errorf("Display = %q, want %q", display, "hearth-loom [agent-aa9752676264a68dd]")
	}

	// Nested deeper inside the worktree → worktree string keeps the
	// agent leaf plus the sub-path.
	deep := filepath.Join(agentDir, "internal", "hooks")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	can, wt = Resolve(deep)
	if can != "hearth-loom" || wt != filepath.Join("agent-aa9752676264a68dd", "internal", "hooks") {
		t.Errorf("deep: got (%q, %q), want (hearth-loom, agent-aa9752676264a68dd/internal/hooks)", can, wt)
	}
}

// TestResolve_ClaudeWorktreesLayout pins the canonical
// <project>/.claude/worktrees/<agent> pattern: cwd is 4-deep but the
// resolver must still return (project, agent) instead of (project, ".claude").
// Repro: live agent.activity emission from nervous-bus showing
// worktree=".claude" before this fix.
func TestResolve_ClaudeWorktreesLayout(t *testing.T) {
	root := makeFakeProjects(t)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	// Build the canonical Claude worktree layout under nervous-bus.
	agentDir := filepath.Join(root, "nervous-bus", ".claude", "worktrees", "agent-a69bbd428f3a2275a")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	can, wt := Resolve(agentDir)
	if can != "nervous-bus" || wt != "agent-a69bbd428f3a2275a" {
		t.Errorf("got (%q, %q), want (nervous-bus, agent-a69bbd428f3a2275a)", can, wt)
	}

	// Nested deeper inside the worktree → worktree string keeps the
	// agent leaf plus the sub-path so subscribers can still distinguish.
	deep := filepath.Join(agentDir, "deep", "nested")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	can, wt = Resolve(deep)
	if can != "nervous-bus" || wt != filepath.Join("agent-a69bbd428f3a2275a", "deep", "nested") {
		t.Errorf("deep: got (%q, %q), want (nervous-bus, agent-a69bbd428f3a2275a/deep/nested)", can, wt)
	}
}

// TestResolve_PreservesPriorBehaviour pins that adding .claude/worktrees
// detection did not regress the existing direct-checkout, bare-worktree,
// or .git-anchored cases.
func TestResolve_PreservesPriorBehaviour(t *testing.T) {
	root := makeFakeProjects(t)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	cases := []struct {
		cwd, wantCan, wantWT string
	}{
		// hearth-loom bare-worktree layout still resolves to wt name
		{filepath.Join(root, "hearth-loom", "main"), "hearth-loom", "main"},
		// tengine direct-checkout still has empty worktree
		{filepath.Join(root, "tengine"), "tengine", ""},
	}
	for _, c := range cases {
		can, wt := Resolve(c.cwd)
		if can != c.wantCan || wt != c.wantWT {
			t.Errorf("Resolve(%q) = (%q, %q), want (%q, %q)", c.cwd, can, wt, c.wantCan, c.wantWT)
		}
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		canonical, worktree, want string
	}{
		{"hearth-loom", "main", "hearth-loom [main]"},
		{"hearth-loom", "wt-foo", "hearth-loom [wt-foo]"},
		{"tengine", "", "tengine"},
		{"hearth-loom", "hearth-loom", "hearth-loom"}, // same name → no brackets
	}
	for _, c := range cases {
		if got := Display(c.canonical, c.worktree); got != c.want {
			t.Errorf("Display(%q, %q) = %q, want %q", c.canonical, c.worktree, got, c.want)
		}
	}
}
