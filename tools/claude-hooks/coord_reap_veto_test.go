package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// settle backdates a lane's tree and git HEAD/reflog so it reads as idle.
func (e *reapEnv) settle(p string) {
	old := time.Now().Add(-3 * time.Hour)
	gd := strings.TrimSpace(reapGit(e.t, p, "rev-parse", "--absolute-git-dir"))
	for _, f := range []string{filepath.Join(gd, "HEAD"), filepath.Join(gd, "logs", "HEAD")} {
		if err := os.Chtimes(f, old, old); err != nil {
			e.t.Fatal(err)
		}
	}
	var paths []string
	_ = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != p && d.Name() == ".git" {
			return nil
		}
		if err == nil {
			paths = append(paths, path)
		}
		return nil
	})
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], old, old); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *reapEnv) assertKept(res *coordReapResult, p, reason string) {
	e.t.Helper()
	if len(res.Kept) != 1 || res.Kept[0].Path != p || !strings.Contains(strings.Join(res.Kept[0].Failing, ";"), reason) {
		e.t.Fatalf("want kept %q: %s", reason, coordReapText(res, time.Now(), true))
	}
	if len(res.Reaped)+len(res.Reapable)+len(res.Festering) != 0 || len(e.mutations()) != 0 || !exists(p) {
		e.t.Fatalf("vetoed tree touched: %s mutations=%v", coordReapText(res, time.Now(), true), e.mutations())
	}
}

func TestReapKeepMarkerInTreeVetoes(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	if err := os.WriteFile(filepath.Join(p, ".keep-worktree"), []byte("frozen actor"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(p)
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "retained"})
	e.assertKept(e.reap(""), p, "frozen actor")
}

func TestReapKeepMarkerInGitDirVetoes(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	gd := strings.TrimSpace(reapGit(t, p, "rev-parse", "--absolute-git-dir"))
	if err := os.WriteFile(filepath.Join(gd, "keep-worktree"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(p)
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	e.assertKept(e.reap(""), p, "keep marker")
}

func TestReapKeepMarkerVetoesAgentWorktree(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	lane := e.lane("lane1")
	agent := e.laneAt(filepath.Join(e.cfg.ReapAgentRoot, "agent-keep1"), "xwt/agent-keep1")
	if err := os.WriteFile(filepath.Join(agent, ".keep-worktree"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(agent)
	e.setWorkers(reapWorkerSpec{"ctx_1", lane, "succeeded", "released"})
	res := e.reap("")
	kept := false
	for _, it := range res.Kept {
		kept = kept || it.Path == agent
	}
	if !kept || !exists(agent) {
		t.Fatalf("agent worktree not kept: %s", coordReapText(res, time.Now(), true))
	}
	for _, c := range e.calls() {
		if strings.Contains(c, agent) {
			t.Fatalf("agent worktree touched: %s", c)
		}
	}
	for _, it := range res.Reaped {
		if it.Path == agent {
			t.Fatalf("agent worktree reaped")
		}
	}
}

func TestReapNestedRecentChangeVetoes(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	deep := filepath.Join(p, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(deep, "out.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every ancestor is aged, so only the nested file is recent.
	e.settle(p)
	now := time.Now()
	if err := os.Chtimes(f, now, now); err != nil {
		t.Fatal(err)
	}
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	e.assertKept(e.reap(""), p, "a/b/c/out.log")
}

func TestReapRecentGitReflogVetoes(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	gd := strings.TrimSpace(reapGit(t, p, "rev-parse", "--absolute-git-dir"))
	now := time.Now()
	if err := os.Chtimes(filepath.Join(gd, "logs", "HEAD"), now, now); err != nil {
		t.Fatal(err)
	}
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	e.assertKept(e.reap(""), p, "git logs/HEAD")
}

func TestReapSkippedDirChangeDoesNotVeto(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	nm := filepath.Join(p, "node_modules", "pkg")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "x.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, ".gitignore"), []byte("node_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reapGit(t, p, "add", ".gitignore")
	reapGit(t, p, "commit", "-m", "ignore")
	reapGit(t, p, "push", "origin", "HEAD:integ")
	e.settle(p)
	now := time.Now()
	if err := os.Chtimes(filepath.Join(nm, "x.js"), now, now); err != nil {
		t.Fatal(err)
	}
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped) != 1 || len(res.Kept) != 0 {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
}

func TestReapSymlinkedCwdBlocks(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	link := filepath.Join(e.dir, "viaLink")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.Dir = link
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	// Probe against the symlinked spelling of the worktree path.
	if !coordCwdInside(cwd, link) {
		t.Fatalf("cwd %q not matched through symlink %q", cwd, link)
	}
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped) != 0 || len(e.mutations()) != 0 || !exists(p) {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
}

// A tree that gains a keep marker between scan and apply is kept.
func TestReapApplyRetestsVeto(t *testing.T) {
	e := newReapEnv(t, reapOrders+`reap = "suggest"`+"\n")
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reapable) != 1 {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
	if err := os.WriteFile(filepath.Join(p, ".keep-worktree"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	coordReapApply(context.Background(), e.runDir(), res)
	if len(res.Reaped) != 0 || len(res.Kept) != 1 || len(e.mutations()) != 0 || !exists(p) {
		t.Fatalf("apply: %s mutations=%v", coordReapText(res, time.Now(), true), e.mutations())
	}
}

func TestReapIdleLandedCleanTreeIsRemoved(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped) != 1 || len(res.Kept) != 0 {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
	if m := e.mutations(); len(m) != 1 || !strings.HasPrefix(m[0], "worktree rm") {
		t.Fatalf("mutations: %v", m)
	}
}
