package main

// Tests for the worktree-isolation guard family. Fixtures live under
// t.TempDir() (i.e. /tmp), which the v2 guards treat as EPHEMERAL and
// exempt — so every blocking test sets WORKTREE_GUARD_STRICT_TMP=1, and
// dedicated tests verify the exemption with it unset. HOME is redirected
// to the fixture so freeze markers and logs never touch the real ones.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type wgFixture struct {
	base    string // fixture root
	primary string // shared primary checkout (.git dir)
	wt1     string // worktree of primary (.git file)
	clone   string // independent primary checkout (scratch clone)
	scratch string // plain non-repo dir
}

func newWgFixture(t *testing.T) *wgFixture {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	t.Setenv("WORKTREE_GUARD_STRICT_TMP", "1")

	f := &wgFixture{
		base:    base,
		primary: filepath.Join(base, "primary"),
		wt1:     filepath.Join(base, "wt1"),
		clone:   filepath.Join(base, "clone"),
		scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{
		filepath.Join(f.primary, ".git"),
		filepath.Join(f.primary, ".planning"),
		filepath.Join(f.primary, ".git", "worktrees", "wt1"),
		filepath.Join(f.clone, ".git"),
		f.wt1, f.scratch, home,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(f.primary, "code.rs"), "fn main() {}\n")
	// Real repos always have .git/HEAD; repoOf requires it (a stray empty
	// .git dir must not read as a repo).
	mustWrite(t, filepath.Join(f.primary, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(f.clone, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(f.wt1, ".git"),
		"gitdir: "+f.primary+"/.git/worktrees/wt1\n")
	// Resolve symlinks once so expectations match guard-side realpaths.
	f.primary = realpathBest(f.primary)
	f.wt1 = realpathBest(f.wt1)
	f.clone = realpathBest(f.clone)
	f.scratch = realpathBest(f.scratch)
	return f
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *wgFixture) freeze(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(wgFreezeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, freezeMarkerPath(root), `{"repo":"`+root+`"}`)
}

func TestBashGuardViolations(t *testing.T) {
	f := newWgFixture(t)
	f.freeze(t, f.primary)

	cases := []struct {
		name   string
		cmd    string
		cwd    string
		agent  bool
		strict string // "" keeps fixture default ("1")
		block  bool
		rule   string // "" means not checked; else every violation's Rule must equal this
	}{
		// --- the real-world false positives that motivated v2 ---
		{name: "cd-then-relative-redirect-outside-repo",
			cmd:   "cd " + f.scratch + " && chromium --dump-dom http://x/ 2>/dev/null > dom.html",
			cwd:   f.primary, block: false},
		{name: "var-assignment-redirect-outside-repo",
			cmd:   "S=" + f.scratch + "; chromium > $S/dom.html",
			cwd:   f.primary, block: false},
		{name: "unset-var-redirect-skipped",
			cmd:   "foo > $WTG_UNSET_XYZ/dom.html",
			cwd:   f.primary, block: false},
		{name: "git-stash-list-readonly",
			cmd:   "git status --short | head -20; git stash list | head",
			cwd:   f.primary, block: false},
		{name: "git-add-dry-run",
			cmd:   "git add --dry-run .",
			cwd:   f.primary, block: false},
		{name: "inline-wave-freeze-off-lifts",
			cmd:   "wave-freeze off " + f.primary + " && git add code.rs && git commit -m x",
			cwd:   f.primary, block: false},
		{name: "cd-into-clone-escapes-frozen-cwd",
			cmd:   "cd " + f.clone + " && git merge --ff-only origin/main",
			cwd:   f.primary, block: false},
		{name: "stderr-fd-redirect-ignored",
			cmd:   "echo err >&2",
			cwd:   f.primary, agent: true, block: false},
		{name: "dev-null-and-fd-dup-ignored",
			cmd:   "foo > /dev/null 2>&1",
			cwd:   f.primary, agent: true, block: false},
		{name: "cp-source-in-primary-dest-outside",
			cmd:   "cp " + f.primary + "/code.rs " + f.scratch + "/copy.rs",
			cwd:   f.primary, agent: true, block: false},
		{name: "sed-script-token-not-a-target",
			cmd:   "sed -i s/a/b/ " + f.scratch + "/nonexistent.rs",
			cwd:   f.primary, agent: true, block: false},
		{name: "sed-without-inplace-writes-nothing",
			cmd:   "sed s/a/b/ code.rs",
			cwd:   f.primary, agent: true, block: false},
		{name: "rm-nonexistent-noop",
			cmd:   "rm -f no-such-file.tmp",
			cwd:   f.primary, agent: true, block: false},
		{name: "heredoc-sql-comparison-operators-not-redirects",
			cmd: "psql <<EOF\n" +
				"SELECT * FROM trades WHERE trade_date >= '2026-08-10'\n" +
				"  AND (yes_levels->0->>'Qty')::double precision > 0;\n" +
				"EOF\n",
			cwd: f.primary, agent: true, block: false},
		{name: "heredoc-quoted-tag-sql",
			cmd: "psql <<'SQL'\n" +
				"SELECT 1 WHERE x >= 0;\n" +
				"SQL\n",
			cwd: f.primary, agent: true, block: false},
		{name: "heredoc-dash-tag-indented-terminator",
			cmd: "cat <<-EOF\n" +
				"\tvalue >= 5\n" +
				"\tEOF\n",
			cwd: f.primary, agent: true, block: false},

		// --- true positives that must survive v2 ---
		{name: "redirect-into-frozen-primary",
			cmd:   "echo x > out.log",
			cwd:   f.primary, block: true, rule: "wave-freeze"},
		{name: "agent-redirect-into-primary-abs",
			cmd:   "echo hi > " + f.primary + "/out.log",
			cwd:   f.wt1, agent: true, block: true, rule: "subagent"},
		{name: "agent-git-commit-in-primary",
			cmd:   "git commit -m x",
			cwd:   f.primary, agent: true, block: true, rule: "subagent"},
		{name: "git-add-under-freeze",
			cmd:   "git add .",
			cwd:   f.primary, block: true, rule: "wave-freeze"},
		{name: "git-stash-pop-mutates",
			cmd:   "git stash pop",
			cwd:   f.primary, block: true},
		{name: "sed-inplace-existing-file-in-primary",
			cmd:   "sed -i s/a/b/ code.rs",
			cwd:   f.primary, agent: true, block: true},
		{name: "pipe-to-tee-into-primary",
			cmd:   "cat code.rs | tee " + f.primary + "/out2.log",
			cwd:   f.scratch, agent: true, block: true},
		{name: "cp-dest-in-primary",
			cmd:   "cp " + f.scratch + "/x " + f.primary + "/code2.rs",
			cwd:   f.scratch, agent: true, block: true},
		{name: "mv-inside-primary",
			cmd:   "mv code.rs code_old.rs",
			cwd:   f.primary, block: true},
		{name: "var-expansion-into-primary-still-caught",
			cmd:   "P=" + f.primary + "; echo x > $P/out.log",
			cwd:   f.scratch, agent: true, block: true},
		{name: "cd-into-primary-then-relative-redirect",
			cmd:   "cd " + f.primary + " && echo x > out.log",
			cwd:   f.scratch, agent: true, block: true},
		{name: "inline-lift-does-not-unblock-subagent",
			cmd:   "wave-freeze off " + f.primary + " && git add code.rs",
			cwd:   f.primary, agent: true, block: true},
		{name: "heredoc-with-genuine-redirect-outside-body-still-blocks",
			cmd: "cat <<EOF > " + f.primary + "/out.log\n" +
				"trade_date >= '2026-08-10'\n" +
				"EOF\n",
			cwd: f.scratch, agent: true, block: true},

		// --- allowlist + worktree paths stay open ---
		{name: "md-write-allowlisted",
			cmd:   "echo x > notes.md",
			cwd:   f.primary, block: false},
		{name: "planning-write-allowlisted",
			cmd:   "tee .planning/wave.txt",
			cwd:   f.primary, block: false},
		{name: "worktree-cwd-relative-ops-free",
			cmd:   "git commit -m x && echo y > out.log",
			cwd:   f.wt1, agent: true, block: false},
		{name: "git-C-worktree-mutation-fine",
			cmd:   "git -C " + f.wt1 + " checkout -- code.rs",
			cwd:   f.primary, agent: true, block: false},
		{name: "git-C-unresolvable-var-fails-open",
			cmd:   `for w in x y; do git -C "$w" status --short; done`,
			cwd:   f.primary, block: false},

		// --- ephemeral exemption (strict off) ---
		{name: "ephemeral-primary-agent-git-allowed",
			cmd:   "git commit -m x",
			cwd:   f.clone, agent: true, strict: "0", block: false},
		{name: "ephemeral-primary-agent-redirect-allowed",
			cmd:   "echo hi > " + f.primary + "/out.log",
			cwd:   f.wt1, agent: true, strict: "0", block: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.strict != "" {
				t.Setenv("WORKTREE_GUARD_STRICT_TMP", tc.strict)
			}
			got := bashGuardViolations(tc.cmd, tc.cwd, tc.agent)
			if (len(got) > 0) != tc.block {
				t.Errorf("cmd=%q cwd=%q agent=%v: got violations %v, want block=%v",
					tc.cmd, tc.cwd, tc.agent, got, tc.block)
			}
			if tc.rule != "" {
				for _, v := range got {
					if v.Rule != tc.rule {
						t.Errorf("cmd=%q: violation Rule=%q, want %q (%+v)",
							tc.cmd, v.Rule, tc.rule, v)
					}
				}
				if got := bashGuardRules(got); got != tc.rule {
					t.Errorf("cmd=%q: bashGuardRules=%q, want %q", tc.cmd, got, tc.rule)
				}
			}
		})
	}
}

// TestBashGuardRules covers the log-line aggregation directly: dedup,
// order-preservation, and the mixed subagent+wave-freeze case a subagent
// running during a freeze can trip (each violation's Rule is "subagent"
// per verdictFor's precedence, but the helper must not assume a single
// rule ever reaches it — a future verdictFor change could emit both).
func TestBashGuardRules(t *testing.T) {
	cases := []struct {
		name string
		in   []bashViolation
		want string
	}{
		{name: "empty", in: nil, want: ""},
		{name: "single-subagent",
			in:   []bashViolation{{Rule: "subagent"}},
			want: "subagent"},
		{name: "dedup-same-rule-twice",
			in:   []bashViolation{{Rule: "wave-freeze"}, {Rule: "wave-freeze"}},
			want: "wave-freeze"},
		{name: "order-preserved-mixed",
			in:   []bashViolation{{Rule: "subagent"}, {Rule: "wave-freeze"}, {Rule: "subagent"}},
			want: "subagent,wave-freeze"},
		{name: "empty-rule-skipped",
			in:   []bashViolation{{Rule: ""}, {Rule: "subagent"}},
			want: "subagent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bashGuardRules(tc.in); got != tc.want {
				t.Errorf("bashGuardRules(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteGuardVerdict(t *testing.T) {
	f := newWgFixture(t)

	target := filepath.Join(f.primary, "src.rs")
	if v, _ := writeGuardVerdict(target, f.primary, true); v != "subagent" {
		t.Errorf("agent write to primary: got %q, want subagent", v)
	}
	if v, _ := writeGuardVerdict(target, f.wt1, false); v != "anchoring" {
		t.Errorf("worktree-cwd write to primary: got %q, want anchoring", v)
	}
	// Cross-repo: a worktree of ONE repo writing a DIFFERENT repo's primary
	// is a deliberate ops edit, not a main-anchoring leak — allow-with-log.
	if v, _ := writeGuardVerdict(filepath.Join(f.clone, "src.rs"), f.wt1, false); v != "cross-repo" {
		t.Errorf("cross-repo write from worktree cwd: got %q, want cross-repo", v)
	}
	// Unresolvable worktree primary (broken gitdir pointer): fail closed to
	// the original anchoring block.
	wtBroken := filepath.Join(f.base, "wt-broken")
	if err := os.MkdirAll(wtBroken, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(wtBroken, ".git"), "gitdir: /nonexistent/gitdir\n")
	if v, _ := writeGuardVerdict(target, realpathBest(wtBroken), false); v != "anchoring" {
		t.Errorf("broken-pointer worktree cwd: got %q, want anchoring (fail closed)", v)
	}
	if v, _ := writeGuardVerdict(filepath.Join(f.wt1, "src.rs"), f.wt1, true); v != "" {
		t.Errorf("agent write to own worktree: got %q, want allow", v)
	}
	if v, _ := writeGuardVerdict(filepath.Join(f.primary, "notes.md"), f.primary, true); v != "" {
		t.Errorf("md write: got %q, want allow", v)
	}
	f.freeze(t, f.primary)
	if v, _ := writeGuardVerdict(target, f.primary, false); v != "freeze" {
		t.Errorf("lead write under freeze: got %q, want freeze", v)
	}
	t.Setenv("WORKTREE_GUARD_STRICT_TMP", "0")
	if v, _ := writeGuardVerdict(target, f.primary, true); v != "" {
		t.Errorf("agent write to ephemeral primary: got %q, want allow", v)
	}
}

func TestReplaceRootRefs(t *testing.T) {
	root := "/home/u/projects/kb"
	wt := "/home/u/projects/kb/.claude/worktrees/agent-x"
	cases := []struct{ in, want string }{
		{"see /home/u/projects/kb/src/a.rs.", "see " + wt + "/src/a.rs."},
		{"root is /home/u/projects/kb, ok", "root is " + wt + ", ok"},
		{"sibling /home/u/projects/kb-tools/x stays", "sibling /home/u/projects/kb-tools/x stays"},
		{"already " + wt + "/f.rs", "already " + wt + "/f.rs"},
		{"/home/u/projects/kb", wt},
	}
	for _, tc := range cases {
		if got := replaceRootRefs(tc.in, root, wt); got != tc.want {
			t.Errorf("replaceRootRefs(%q):\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestShellSplitAndExpand(t *testing.T) {
	toks, err := shellSplit(`grep -oE "<(h1|h2)[^>]*>" $S/dom.html | head -25`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"grep", "-oE", "<(h1|h2)[^>]*>", "$S/dom.html", "|", "head", "-25"}
	if strings.Join(toks, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("shellSplit: got %v, want %v", toks, want)
	}
	if _, err := shellSplit(`echo "unbalanced`); err == nil {
		t.Error("shellSplit: expected error on unbalanced quote")
	}

	s := "/tmp/x"
	vars := map[string]*string{"S": &s, "U": nil}
	if v, ok := expandTok("$S/dom.html", vars); !ok || v != "/tmp/x/dom.html" {
		t.Errorf("expandTok known var: got %q ok=%v", v, ok)
	}
	if _, ok := expandTok("$U/f", vars); ok {
		t.Error("expandTok known-unknown var should fail")
	}
	if _, ok := expandTok("$WTG_DEFINITELY_UNSET/f", vars); ok {
		t.Error("expandTok unset var should fail")
	}
	if _, ok := expandTok("$(date)/f", vars); ok {
		t.Error("expandTok command substitution should fail")
	}
}

func TestRepoOfIgnoresInvalidGitDir(t *testing.T) {
	f := newWgFixture(t)
	// A stray EMPTY .git dir (the /tmp/.git incident, 2026-07-21) must not
	// make its parent a phantom primary checkout.
	stray := filepath.Join(f.scratch, "straydir")
	if err := os.MkdirAll(filepath.Join(stray, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if root, kind := repoOf(filepath.Join(stray, "x.log")); kind != "" {
		t.Errorf("repoOf(under empty .git): got (%q,%q), want not-a-repo", root, kind)
	}
	// A valid .git dir (with HEAD) still reads as primary.
	if root, kind := repoOf(filepath.Join(f.primary, "code.rs")); kind != "primary" || root != f.primary {
		t.Errorf("repoOf(primary): got (%q,%q), want (%q,primary)", root, kind, f.primary)
	}
}

func TestIsEphemeralExactRoot(t *testing.T) {
	t.Setenv("WORKTREE_GUARD_STRICT_TMP", "")
	for _, root := range []string{"/tmp", "/tmp/scratch-clone", "/var/tmp", "/dev/shm/x"} {
		if !isEphemeral(root) {
			t.Errorf("isEphemeral(%q) = false, want true", root)
		}
	}
	if isEphemeral("/home/u/projects/kb") {
		t.Error("isEphemeral(real project root) = true, want false")
	}
	t.Setenv("WORKTREE_GUARD_STRICT_TMP", "1")
	if isEphemeral("/tmp/scratch-clone") {
		t.Error("isEphemeral under STRICT_TMP=1 must be false")
	}
}

func TestPrimaryRootForWorktree(t *testing.T) {
	f := newWgFixture(t)
	if got := primaryRootFor(f.wt1); got != f.primary {
		t.Errorf("primaryRootFor(worktree): got %q, want %q", got, f.primary)
	}
	if got := primaryRootFor(f.primary); got != f.primary {
		t.Errorf("primaryRootFor(primary): got %q, want %q", got, f.primary)
	}
}
