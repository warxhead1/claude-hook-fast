package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeT(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type verdictRepo struct{ wt string }

// newVerdictRepo builds origin + a worktree on main with a base commit.
func newVerdictRepo(t *testing.T) verdictRepo {
	t.Helper()
	root := t.TempDir()
	origin, wt := filepath.Join(root, "origin.git"), filepath.Join(root, "wt")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, wt, "init", "-b", "main")
	writeT(t, wt, "a.txt", "base\n")
	writeT(t, wt, "src/keep.go", "package src\n")
	gitT(t, wt, "add", ".")
	gitT(t, wt, "commit", "-m", "base")
	gitT(t, wt, "remote", "add", "origin", origin)
	gitT(t, wt, "push", "origin", "main")
	return verdictRepo{wt: wt}
}

func (h *coordHarness) setWorker(wt, branch, spec string) {
	ws := map[string]any{"ok": true, "result": map[string]any{
		"worker":   map[string]any{"worktreeId": "repo::" + wt},
		"terminal": map[string]any{"worktreePath": wt, "branch": "refs/heads/" + branch}}}
	b, _ := json.Marshal(ws)
	_ = os.WriteFile(filepath.Join(h.dir, "worker_show.json"), b, 0o644)
	tl := map[string]any{"ok": true, "result": map[string]any{"tasks": []map[string]any{
		{"id": "task_a", "task_title": "Hero build: spinner", "spec": spec}}}}
	b, _ = json.Marshal(tl)
	_ = os.WriteFile(filepath.Join(h.dir, "task_list.json"), b, 0o644)
}

func verdictFor(h *coordHarness, integration string) string {
	m := coordMsg{ID: "msg_done", Type: "worker_done", From: "term_lane",
		Payload: `{"taskId":"task_a","dispatchId":"ctx_1"}`}
	return coordVerdict(context.Background(), h.cfg, "run_x", m, integration)
}

func TestCoordVerdict(t *testing.T) {
	const scope = "Do the thing.\n- FILE SCOPE (write): src/**, docs/lane/** ONLY. Nothing else.\n- FILE SCOPE bounds what you WRITE, never what you READ.\n"
	cases := []struct {
		name  string
		spec  string
		setup func(t *testing.T, wt string)
		want  []string
		deny  []string
	}{
		{"clean-merge-pushed", scope, func(t *testing.T, wt string) {
			gitT(t, wt, "checkout", "-b", "lane")
			writeT(t, wt, "src/x.go", "package src\n")
			writeT(t, wt, "src/x_test.go", "package src\n")
			gitT(t, wt, "add", ".")
			gitT(t, wt, "commit", "-m", "lane")
			gitT(t, wt, "push", "origin", "lane")
		}, []string{"pushed: yes", "merge into main: clean", "2 file(s)", "tests touched: 1", "scope: all changed files inside FILE SCOPE"}, []string{"OUTSIDE"}},
		{"conflict", scope, func(t *testing.T, wt string) {
			gitT(t, wt, "checkout", "-b", "lane")
			writeT(t, wt, "a.txt", "lane edit\n")
			gitT(t, wt, "commit", "-am", "lane")
			gitT(t, wt, "push", "origin", "lane")
			gitT(t, wt, "checkout", "main")
			writeT(t, wt, "a.txt", "main edit\n")
			gitT(t, wt, "commit", "-am", "main moves")
			gitT(t, wt, "checkout", "lane")
		}, []string{"CONFLICTS in 1 path(s): a.txt", "pushed: yes"}, []string{"clean"}},
		{"out-of-scope-file", scope, func(t *testing.T, wt string) {
			gitT(t, wt, "checkout", "-b", "lane")
			writeT(t, wt, "src/ok.go", "package src\n")
			writeT(t, wt, "Content/stray.uasset", "x")
			gitT(t, wt, "add", ".")
			gitT(t, wt, "commit", "-m", "lane")
			gitT(t, wt, "push", "origin", "lane")
		}, []string{"1 file(s) OUTSIDE FILE SCOPE: Content/stray.uasset", "merge into main: clean"}, []string{"src/ok.go,"}},
		{"unpushed-branch", scope, func(t *testing.T, wt string) {
			gitT(t, wt, "checkout", "-b", "lane")
			writeT(t, wt, "src/x.go", "package src\n")
			gitT(t, wt, "add", ".")
			gitT(t, wt, "commit", "-m", "lane")
		}, []string{"pushed: NO (branch lane absent on origin)"}, nil},
		{"pushed-but-behind-local", scope, func(t *testing.T, wt string) {
			gitT(t, wt, "checkout", "-b", "lane")
			writeT(t, wt, "src/x.go", "package src\n")
			gitT(t, wt, "add", ".")
			gitT(t, wt, "commit", "-m", "one")
			gitT(t, wt, "push", "origin", "lane")
			writeT(t, wt, "src/y.go", "package src\n")
			gitT(t, wt, "add", ".")
			gitT(t, wt, "commit", "-m", "two")
		}, []string{"pushed: NO (origin/lane is at"}, nil},
		{"no-file-scope-line", "just do it, no scope declared", func(t *testing.T, wt string) {
			gitT(t, wt, "checkout", "-b", "lane")
			writeT(t, wt, "src/x.go", "package src\n")
			gitT(t, wt, "add", ".")
			gitT(t, wt, "commit", "-m", "lane")
			gitT(t, wt, "push", "origin", "lane")
		}, []string{"scope: no FILE SCOPE line"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCoordHarness(t, "term_coord")
			r := newVerdictRepo(t)
			c.setup(t, r.wt)
			h.setWorker(r.wt, "lane", c.spec)
			got := verdictFor(h, "main")
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("verdict missing %q:\n%s", w, got)
				}
			}
			for _, d := range c.deny {
				if strings.Contains(got, d) {
					t.Errorf("verdict contains %q:\n%s", d, got)
				}
			}
		})
	}
}

func TestCoordVerdictUnavailableAndNoIntegration(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	if got := verdictFor(h, "main"); !strings.HasPrefix(got, "verdict unavailable:") {
		t.Fatalf("no worker-show output should be unavailable, got %q", got)
	}
	h.setWorker(filepath.Join(h.dir, "nonexistent"), "lane", "")
	if got := verdictFor(h, "main"); !strings.HasPrefix(got, "verdict unavailable: worktree") {
		t.Fatalf("missing worktree: %q", got)
	}
	r := newVerdictRepo(t)
	h.setWorker(r.wt, "main", "")
	got := verdictFor(h, "")
	if !strings.Contains(got, "skipped (no integration_branch") || !strings.Contains(got, "pushed: yes") {
		t.Fatalf("no-integration verdict: %s", got)
	}
}

func TestCoordWakeWorkerDoneCarriesVerdict(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	r := newVerdictRepo(t)
	gitT(t, r.wt, "checkout", "-b", "lane")
	writeT(t, r.wt, "src/x.go", "package src\n")
	gitT(t, r.wt, "add", ".")
	gitT(t, r.wt, "commit", "-m", "lane")
	h.setWorker(r.wt, "lane", "FILE SCOPE (write): src/**")
	h.withOrders(`integration_branch = "main"`)
	h.wakeWith("d1", coordMsgJSON("msg_d", "worker_done", "term_lane", "done", "report"))
	out := h.errb.String()
	for _, w := range []string{"landing verdict:", "pushed: NO", "merge into main: clean", "1 file(s)"} {
		if !strings.Contains(out, w) {
			t.Fatalf("wake missing %q:\n%s", w, out)
		}
	}
}

func TestCoordScopeGlobs(t *testing.T) {
	spec := "- FILE SCOPE (write): tools/hearth_progress.py, tools/tests/test_{gate_summary,progress_check}*.py, scripts/check.sh (ONE hook line only), scripts/cine_*.sh (new), docs/design/wear.md. Nothing else. Do not edit x/.\n- FILE SCOPE bounds what you WRITE."
	globs, found := coordScopeGlobs(spec)
	want := []string{"tools/hearth_progress.py", "tools/tests/test_{gate_summary,progress_check}*.py", "scripts/check.sh", "scripts/cine_*.sh", "docs/design/wear.md"}
	if !found || fmt.Sprint(globs) != fmt.Sprint(want) {
		t.Fatalf("globs %q found=%v\nwant %q", globs, found, want)
	}
	oos := coordOutOfScope([]string{"tools/hearth_progress.py", "tools/tests/test_gate_summary_x.py", "scripts/cine_a.sh", "scripts/other.sh", "docs/design/wear.md"}, globs)
	if fmt.Sprint(oos) != "[scripts/other.sh]" {
		t.Fatalf("out of scope: %v", oos)
	}
	if _, found := coordScopeGlobs("- FILE SCOPE bounds what you WRITE, never what you READ."); found {
		t.Fatal("boilerplate line treated as a scope declaration")
	}
	if g, found := coordScopeGlobs("FILE SCOPE (write): a/**. Nothing else."); !found || len(g) != 1 || g[0] != "a/**" {
		t.Fatalf("trailing sentence: %q", g)
	}
}
