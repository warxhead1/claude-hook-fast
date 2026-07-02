package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsGitRepo(t *testing.T) {
	tmp := t.TempDir()
	if isGitRepo(tmp) {
		t.Error("expected false for a plain temp dir with no .git ancestor")
	}

	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(repo) {
		t.Error("expected true when cwd itself has a .git dir")
	}

	nested := filepath.Join(repo, "crates", "hearth-api", "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(nested) {
		t.Error("expected true when a .git ancestor exists a few levels up")
	}

	if isGitRepo("") {
		t.Error("expected false for empty cwd")
	}
}

func TestAlreadyIsolatedCWD(t *testing.T) {
	cases := []struct {
		cwd  string
		want bool
	}{
		{"/home/eric/projects/hearth", false},
		{"/home/eric/projects/hearth-loom/main", false}, // operator branch layout, NOT per-dispatch isolation
		{"/home/eric/projects/hearth-loom/wt/some-branch", false},
		{"/home/eric/projects/hearth/.claude/worktrees/agent-abc123", true},
		{"/home/eric/projects/hearth/.claude/worktrees/agent-abc123/crates", true},
		{"/home/eric/projects/hearth/.worktrees/task/some-task", true},
	}
	for _, c := range cases {
		if got := alreadyIsolatedCWD(c.cwd); got != c.want {
			t.Errorf("alreadyIsolatedCWD(%q) = %v, want %v", c.cwd, got, c.want)
		}
	}
}

func TestCheckAgentNoIsolation(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "hearth")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nonGit := filepath.Join(tmp, "not-a-repo")
	if err := os.MkdirAll(nonGit, 0o755); err != nil {
		t.Fatal(err)
	}
	wtDir := filepath.Join(repo, ".claude", "worktrees", "agent-xyz")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cwd  string
		ti   string
		want bool
	}{
		{"missing isolation in shared repo -> block", repo, `{"description":"x"}`, true},
		{"blank isolation in shared repo -> block", repo, `{"isolation":""}`, true},
		{"isolation: worktree present -> pass", repo, `{"isolation":"worktree"}`, false},
		{"isolation: remote present -> pass", repo, `{"isolation":"remote"}`, false},
		{"non-git cwd -> pass", nonGit, `{}`, false},
		{"already inside agent worktree -> pass", wtDir, `{}`, false},
		{"empty tool_input -> fail-open pass", repo, ``, false},
		{"malformed tool_input -> fail-open pass", repo, `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkAgentNoIsolation(c.cwd, []byte(c.ti))
			if got != c.want {
				t.Errorf("checkAgentNoIsolation(%q, %q) = %v, want %v", c.cwd, c.ti, got, c.want)
			}
		})
	}
}

func TestCheckWorkflowPartialIsolation(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "hearth")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	fullyCovered := `{"script":"agent('a', {isolation: 'worktree'}); agent('b', {isolation: 'worktree'})"}`
	partiallyCovered := `{"script":"agent('a', {isolation: 'worktree'}); agent('b', {})"}`
	noneCovered := `{"script":"agent('a', {}); agent('b', {})"}`
	noAgents := `{"script":"log('hi')"}`

	cases := []struct {
		name string
		ti   string
		want bool
	}{
		{"fully covered -> pass", fullyCovered, false},
		{"partially covered -> block", partiallyCovered, true},
		{"none covered -> block", noneCovered, true},
		{"no agent() calls -> pass", noAgents, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkWorkflowPartialIsolation(repo, []byte(c.ti))
			if got != c.want {
				t.Errorf("checkWorkflowPartialIsolation(...) = %v, want %v", got, c.want)
			}
		})
	}
}
