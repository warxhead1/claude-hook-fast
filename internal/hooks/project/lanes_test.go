package project

import (
	"os"
	"path/filepath"
	"testing"
)

func mkWorktree(t *testing.T, dir, gitdir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLaneContainerResolvesToTheRepo(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEARTH_HOOK_DATA2_WORKTREES_ROOT", root)
	mkWorktree(t, filepath.Join(root, "uipolish", "agent-a1"), "/home/x/projects/hearth/.git/worktrees/agent-a1")
	mkWorktree(t, filepath.Join(root, "uipolish", "agent-a2"), "/home/x/projects/hearth/.git/worktrees/agent-a2")
	if can, wt := Resolve(filepath.Join(root, "uipolish", "agent-a1", "src")); can != "hearth" || wt != "agent-a1/src" {
		t.Fatalf("got %q %q", can, wt)
	}
	// a deleted worktree takes the siblings' vote
	if can, _ := Resolve(filepath.Join(root, "uipolish", "agent-gone")); can != "hearth" {
		t.Fatalf("deleted worktree: got %q", can)
	}
}

func TestLaneContainerWithMixedReposKeepsItsName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEARTH_HOOK_DATA2_WORKTREES_ROOT", root)
	mkWorktree(t, filepath.Join(root, "mixed", "a"), "/p/hearth/.git/worktrees/a")
	mkWorktree(t, filepath.Join(root, "mixed", "b"), "/p/kb/.git/worktrees/b")
	if can, _ := Resolve(filepath.Join(root, "mixed", "gone")); can != "mixed" {
		t.Fatalf("got %q", can)
	}
}

func TestBareAndDotGitRepoShapes(t *testing.T) {
	d := t.TempDir()
	mkWorktree(t, filepath.Join(d, "a"), "/p/hearth-loom/.bare/worktrees/a")
	mkWorktree(t, filepath.Join(d, "b"), "/srv/kb.git/worktrees/b")
	if got := repoOfWorktree(filepath.Join(d, "a")); got != "hearth-loom" {
		t.Fatalf("bare: %q", got)
	}
	if got := repoOfWorktree(filepath.Join(d, "b")); got != "kb" {
		t.Fatalf("dot-git: %q", got)
	}
}

func TestOrcaWorkspaceIsItsProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEARTH_HOOK_DATA2_ORCA_ROOT", root)
	if can, wt := Resolve(filepath.Join(root, "app-to-market", "wardrobe", "x")); can != "app-to-market" || wt != "wardrobe/x" {
		t.Fatalf("got %q %q", can, wt)
	}
	if can, _ := Resolve(filepath.Join(root, "_evidence", "x")); can == "_evidence" {
		t.Fatal("evidence dir named as a project")
	}
}

func TestCronLaneIsItsFamily(t *testing.T) {
	cwd := "/home/x/data2/worktrees/cron/auto-nightly-digest-run-12-20261114T0530Z/sub"
	if can, wt := Resolve(cwd); can != "auto-nightly-digest" || wt != "auto-nightly-digest-run-12-20261114T0530Z" {
		t.Fatalf("got %q %q", can, wt)
	}
}
