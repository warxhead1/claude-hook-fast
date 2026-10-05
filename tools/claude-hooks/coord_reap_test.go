package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reapEnv struct {
	*coordHarness
	origin, repo, wt string
}

func reapGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newReapEnv builds a bare origin, a clone with the integration branch
// "integ" pushed, and the standing orders for the run.
func newReapEnv(t *testing.T, orders string) *reapEnv {
	t.Helper()
	h := newCoordHarness(t, "term_coord")
	g := filepath.Join(h.dir, "git")
	e := &reapEnv{coordHarness: h, origin: filepath.Join(g, "origin.git"), repo: filepath.Join(g, "repo"), wt: filepath.Join(g, "wt")}
	for _, d := range []string{e.origin, e.repo, e.wt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reapGit(t, e.origin, "init", "--bare", "-b", "integ")
	reapGit(t, e.repo, "init", "-b", "integ")
	reapGit(t, e.repo, "remote", "add", "origin", e.origin)
	if err := os.WriteFile(filepath.Join(e.repo, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	reapGit(t, e.repo, "add", ".")
	reapGit(t, e.repo, "commit", "-m", "base")
	reapGit(t, e.repo, "push", "origin", "integ")
	if err := os.MkdirAll(e.runDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.runDir(), "standing-orders.toml"), []byte(orders), 0o644); err != nil {
		t.Fatal(err)
	}
	h.cfg.RepoDir = h.dir
	h.cfg.ReapCwd = e.repo
	h.cfg.ReapAgentRoot = filepath.Join(g, "agents") + "/"
	return e
}

const reapOrders = `integration_branch = "integ"` + "\n"

// lane adds a linked worktree on a new branch cut from integ.
func (e *reapEnv) lane(name string) string {
	return e.laneAt(filepath.Join(e.wt, name), name)
}

func (e *reapEnv) laneAt(p, branch string) string {
	reapGit(e.t, e.repo, "worktree", "add", "-b", branch, p, "integ")
	return p
}

func (e *reapEnv) commitIn(p string) {
	if err := os.WriteFile(filepath.Join(p, "new.txt"), []byte("n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	reapGit(e.t, p, "add", ".")
	reapGit(e.t, p, "commit", "-m", "lane work")
}

type reapWorkerSpec struct{ dispatch, path, state, term string }

func (e *reapEnv) setWorkers(ws ...reapWorkerSpec) {
	var recs []map[string]any
	for _, w := range ws {
		recs = append(recs, map[string]any{"dispatchId": w.dispatch, "taskId": "task_a", "runId": "run_x",
			"workerState": w.state, "dispatchStatus": "completed", "terminalState": w.term,
			"agentTerminalHandle": "term_" + w.dispatch,
			"resource":            map[string]any{"worktreeId": "repo1::" + w.path}})
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"workers": recs}})
	if err := os.WriteFile(filepath.Join(e.dir, "worker_list.json"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// ageSeen backdates first-seen so the grace window has passed.
func (e *reapEnv) ageSeen(paths ...string) {
	f := coordLoadReap(e.runDir())
	for _, p := range paths {
		f.FirstSeen[p] = time.Now().Add(-2 * time.Hour).Unix()
	}
	f.save(e.runDir())
}

func (e *reapEnv) reap(mode string) *coordReapResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return coordReap(ctx, e.cfg, "run_x", e.runDir(), mode, true)
}

func (e *reapEnv) mutations() []string {
	var out []string
	for _, c := range e.calls() {
		if strings.HasPrefix(c, "orchestration worker-stop") || strings.HasPrefix(c, "worktree rm") {
			out = append(out, c)
		}
	}
	return out
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestReapMergedCleanStopsThenRemoves(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	q := e.lane("lane2")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "retained"}, reapWorkerSpec{"ctx_2", q, "failed", "released"})
	res := e.reap("")
	if len(res.Reaped) != 2 || len(res.Festering) != 0 {
		t.Fatalf("result: %s", coordReapText(res, time.Now(), true))
	}
	got := e.mutations()
	want := []string{
		"orchestration worker-stop --dispatch ctx_1 --json",
		"worktree rm --worktree path:" + p + " --json",
		"worktree rm --worktree path:" + q + " --json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mutations:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range got {
		if strings.Contains(c, "--force") {
			t.Fatalf("--force used: %s", c)
		}
	}
}

func TestReapStopFailureSkipsRemove(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "retained"})
	if err := os.WriteFile(filepath.Join(e.dir, "stopfail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.reap("")
	if len(res.Reaped) != 0 {
		t.Fatalf("reaped despite stop failure")
	}
	for _, c := range e.mutations() {
		if strings.HasPrefix(c, "worktree rm") {
			t.Fatalf("rm ran after failed stop: %s", c)
		}
	}
}

func TestReapDirtyIsFestering(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	if err := os.WriteFile(filepath.Join(p, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped) != 0 || len(res.Pending) != 1 || !strings.Contains(res.Pending[0].Failing[0], "dirty with 1 files") {
		t.Fatalf("first scan: %s", coordReapText(res, time.Now(), true))
	}
	e.ageSeen(p)
	res = e.reap("")
	if len(res.Festering) != 1 || !strings.Contains(strings.Join(res.Festering[0].Failing, ";"), "dirty with 1 files") {
		t.Fatalf("second scan: %s", coordReapText(res, time.Now(), true))
	}
	if m := e.mutations(); len(m) != 0 {
		t.Fatalf("dirty tree touched: %v", m)
	}
}

func TestReapUnmergedIsFestering(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.commitIn(p)
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	e.reap("")
	e.ageSeen(p)
	res := e.reap("")
	if len(res.Festering) != 1 {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
	f := strings.Join(res.Festering[0].Failing, ";")
	if !strings.Contains(f, "1 commits not in origin/integ") || !strings.Contains(f, "unpushed") {
		t.Fatalf("failing = %q", f)
	}
	if m := e.mutations(); len(m) != 0 {
		t.Fatalf("unmerged tree touched: %v", m)
	}
	// Pushing the branch is not landing it: still festering.
	reapGit(t, p, "push", "origin", "lane1")
	res = e.reap("")
	if len(res.Festering) != 1 || strings.Contains(strings.Join(res.Festering[0].Failing, ";"), "unpushed") {
		t.Fatalf("after push: %s", coordReapText(res, time.Now(), true))
	}
	// Landing it into integ makes it reapable.
	reapGit(t, p, "push", "origin", "lane1:integ")
	res = e.reap("")
	if len(res.Reaped) != 1 {
		t.Fatalf("after landing: %s", coordReapText(res, time.Now(), true))
	}
}

func TestReapCwdInsidePathBlocks(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	cmd := exec.Command("sleep", "60")
	cmd.Dir = p
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped) != 0 || len(res.Pending) != 1 || !strings.Contains(res.Pending[0].Failing[0], "cwd in use by sleep") {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
	if m := e.mutations(); len(m) != 0 {
		t.Fatalf("in-use tree touched: %v", m)
	}
}

func TestReapCoordinatorCwdNeverReaped(t *testing.T) {
	for _, sub := range []string{"", "sub"} {
		e := newReapEnv(t, reapOrders)
		p := e.lane("lane1")
		cwd := p
		if sub != "" {
			cwd = filepath.Join(p, sub)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		e.cfg.ReapCwd = cwd
		e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
		res := e.reap("")
		if len(res.Reaped) != 0 || len(res.Pending) != 1 ||
			!strings.Contains(strings.Join(res.Pending[0].Failing, ";"), "coordinator's cwd") {
			t.Fatalf("sub=%q: %s", sub, coordReapText(res, time.Now(), true))
		}
		if m := e.mutations(); len(m) != 0 {
			t.Fatalf("coordinator cwd tree touched: %v", m)
		}
	}
}

func TestReapNonTerminalWorkerSkipped(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "ready", "active"})
	res := e.reap("")
	if len(res.Reaped)+len(res.Reapable)+len(res.Festering)+len(res.Pending) != 0 || len(e.mutations()) != 0 {
		t.Fatalf("live worker classified: %s", coordReapText(res, time.Now(), true))
	}
}

func TestReapSuggestRunsNothing(t *testing.T) {
	e := newReapEnv(t, reapOrders+`reap = "suggest"`+"\n")
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "retained"})
	res := e.reap("")
	if len(res.Reapable) != 1 || len(res.Reaped) != 0 || len(e.mutations()) != 0 {
		t.Fatalf("scan: %s mutations=%v", coordReapText(res, time.Now(), true), e.mutations())
	}
	want := []string{
		e.cfg.OrcaBin + " orchestration worker-stop --dispatch ctx_1 --json",
		e.cfg.OrcaBin + " worktree rm --worktree path:" + p + " --json",
	}
	if got := res.Reapable[0].cmdLines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("suggested commands: %v", got)
	}
	if !exists(p) {
		t.Fatal("worktree removed in suggest mode")
	}
}

func TestReapOffDoesNothing(t *testing.T) {
	e := newReapEnv(t, reapOrders+`reap = "off"`+"\n")
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "retained"})
	res := e.reap("")
	if res.Mode != "off" || len(res.Reaped)+len(res.Reapable)+len(res.Festering) != 0 {
		t.Fatalf("scan: %+v", res)
	}
	if exists(filepath.Join(e.dir, "calls.log")) {
		t.Fatalf("orca called in off mode: %v", e.calls())
	}
}

func TestReapIntegrationUnsetNothingReapable(t *testing.T) {
	e := newReapEnv(t, "")
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped)+len(res.Reapable) != 0 || len(e.mutations()) != 0 || !strings.Contains(res.Note, "integration_branch") {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
}

func TestReapPrimaryCheckoutIsNeverTouched(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	e.setWorkers(reapWorkerSpec{"ctx_1", e.repo, "succeeded", "released"})
	e.cfg.ReapCwd = e.dir
	res := e.reap("")
	if len(res.Reaped)+len(res.Reapable)+len(res.Festering)+len(res.Pending) != 0 || len(e.mutations()) != 0 {
		t.Fatalf("primary checkout classified: %s", coordReapText(res, time.Now(), true))
	}
}

func TestReapAgentWorktreeRemovedByGit(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	agent := e.laneAt(filepath.Join(e.cfg.ReapAgentRoot, "agent-abc123"), "xwt/agent-abc123")
	e.quiet(agent)
	other := e.laneAt(filepath.Join(e.dir, "git", "notagent", "feature"), "feature")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if len(res.Reaped) != 2 {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
	if exists(agent) {
		t.Fatalf("agent worktree still present")
	}
	if !exists(other) {
		t.Fatal("non-agent worktree outside agent root was removed")
	}
	if out := reapGit(t, e.repo, "branch", "--list", "xwt/agent-abc123"); !strings.Contains(out, "agent-abc123") {
		t.Fatalf("agent branch deleted: %q", out)
	}
	for _, c := range e.mutations() {
		if strings.Contains(c, "agent-abc123") {
			t.Fatalf("agent worktree went through orca: %s", c)
		}
	}
}

func TestReapAgentDirtyIsNotReaped(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	agent := e.laneAt(filepath.Join(e.cfg.ReapAgentRoot, "agent-abc123"), "xwt/agent-abc123")
	e.commitIn(agent)
	e.quiet(agent)
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	e.reap("")
	e.ageSeen(agent)
	res := e.reap("")
	if len(res.Festering) != 1 || res.Festering[0].Kind != "agent" || !exists(agent) {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
}

// quiet backdates a worktree's mtimes so it looks idle for two hours.
func (e *reapEnv) quiet(p string) {
	old := time.Now().Add(-2 * time.Hour)
	gd := strings.TrimSpace(reapGit(e.t, p, "rev-parse", "--absolute-git-dir"))
	paths := []string{p, filepath.Join(gd, "HEAD"), filepath.Join(gd, "index"), filepath.Join(gd, "logs", "HEAD")}
	ents, _ := os.ReadDir(p)
	for _, d := range ents {
		paths = append(paths, filepath.Join(p, d.Name()))
	}
	for _, x := range paths {
		if _, err := os.Stat(x); err == nil {
			_ = os.Chtimes(x, old, old)
		}
	}
}

func TestReapParsesRealWorkerListFixture(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	b, err := os.ReadFile("testdata/worker_list_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "worker_list.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := coordListWorkers(context.Background(), e.cfg, "run_x")
	if err != nil || len(ws) != 16 {
		t.Fatalf("workers=%d err=%v", len(ws), err)
	}
	terminal := 0
	for _, w := range ws {
		if coordReapTerminal(w.WorkerState) {
			terminal++
			if !strings.HasPrefix(w.path(), "/home/eric/data2/worktrees/") {
				t.Fatalf("path %q from %q", w.path(), w.Resource.WorktreeID)
			}
		}
	}
	if terminal != 12 {
		t.Fatalf("terminal=%d, want 12", terminal)
	}
	if res := e.reap(""); len(res.Live) != 4 || res.Gone != 12 || len(e.mutations()) != 0 {
		t.Fatalf("scan: %s", coordReapText(res, time.Now(), true))
	}
}

func TestReapFreshAgentWorktreeIsLeftAlone(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	agent := e.laneAt(filepath.Join(e.cfg.ReapAgentRoot, "agent-live1"), "xwt/agent-live1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	res := e.reap("")
	if !exists(agent) || len(res.Reaped) != 1 || res.Reaped[0].Kind != "orca" {
		t.Fatalf("fresh agent worktree handled: %s", coordReapText(res, time.Now(), true))
	}
	e.quiet(agent)
	res = e.reap("")
	if exists(agent) {
		t.Fatalf("idle merged agent worktree not reaped: %s", coordReapText(res, time.Now(), true))
	}
}

func TestStopGateBlocksOncePerFesteringSet(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.commitIn(p)
	q := e.lane("lane2")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	gate := func() string {
		var out bytes.Buffer
		if rc := coordStopGate(e.cfg, &out, false); rc != 0 {
			t.Fatalf("exit %d", rc)
		}
		return out.String()
	}
	if got := gate(); got != "" {
		t.Fatalf("blocked inside the grace window: %q", got)
	}
	e.ageSeen(p)
	out := gate()
	var d struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Decision != "block" {
		t.Fatalf("expected block, got %q (%v)", out, err)
	}
	for _, want := range []string{"lane1", p, "1 commits not in origin/integ", "merge:", "abandon", "worktree rm --worktree path:" + p} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("reason missing %q:\n%s", want, d.Reason)
		}
	}
	if got := gate(); got != "" {
		t.Fatalf("same set blocked twice: %q", got)
	}
	// A new festering lane changes the set and blocks again.
	e.commitIn(q)
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"}, reapWorkerSpec{"ctx_2", q, "succeeded", "released"})
	e.reap("")
	e.ageSeen(p, q)
	if got := gate(); got == "" {
		t.Fatal("changed set did not block")
	}
	if m := e.mutations(); len(m) != 0 {
		t.Fatalf("gate touched festering lanes: %v", m)
	}
}

func TestStopGateReapsLandedLanesSilently(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	var out bytes.Buffer
	if rc := coordStopGate(e.cfg, &out, false); rc != 0 || out.Len() != 0 {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
	if m := e.mutations(); len(m) != 1 || !strings.HasPrefix(m[0], "worktree rm") {
		t.Fatalf("mutations: %v", m)
	}
}

func TestStopGateReapFailsOpen(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	if err := os.WriteFile(filepath.Join(e.dir, "worker_list.json"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := coordStopGate(e.cfg, &out, false); rc != 0 || out.Len() != 0 {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
}

func TestCoordStatusShowsReapSection(t *testing.T) {
	e := newReapEnv(t, reapOrders)
	p := e.lane("lane1")
	e.commitIn(p)
	e.setWorkers(reapWorkerSpec{"ctx_1", p, "succeeded", "released"})
	e.reap("")
	e.ageSeen(p)
	e.reap("")
	l := coordLoadLedger(e.runDir(), "run_x")
	l.save(e.runDir())
	var out, errb bytes.Buffer
	if rc := coordStatus(e.cfg.StateRoot, "run_x", false, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "FESTERING (1)") || !strings.Contains(out.String(), p) {
		t.Fatalf("status:\n%s", out.String())
	}
}
