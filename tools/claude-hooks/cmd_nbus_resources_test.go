package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

type published struct {
	channel string
	payload map[string]any
	raw     string
}

// capturePublish replaces the detached publisher for the test's duration.
func capturePublish(t *testing.T) func() []published {
	t.Helper()
	var mu sync.Mutex
	var got []published
	prev := nbusPublish
	nbusPublish = func(channel, source, payload string) error {
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Errorf("payload not JSON: %v", err)
		}
		mu.Lock()
		got = append(got, published{channel, m, payload})
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { nbusPublish = prev })
	resetRepoCache()
	t.Cleanup(resetRepoCache)
	return func() []published {
		mu.Lock()
		defer mu.Unlock()
		return append([]published(nil), got...)
	}
}

// repoFixture builds <root>/repos/<name>/.git (dir) plus linked worktrees
// <root>/wts/<wt>/.git (file -> gitdir with commondir), without invoking git.
func repoFixture(t *testing.T, name string, worktrees ...string) (main string, wts map[string]string) {
	t.Helper()
	root := t.TempDir()
	main = filepath.Join(root, "repos", name)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(main, ".git", "worktrees"), 0o755))
	wts = map[string]string{}
	for _, wt := range worktrees {
		dir := filepath.Join(root, "wts", wt)
		gitdir := filepath.Join(main, ".git", "worktrees", wt)
		must(os.MkdirAll(dir, 0o755))
		must(os.MkdirAll(gitdir, 0o755))
		must(os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644))
		must(os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644))
		wts[wt] = dir
	}
	return main, wts
}

func toolInput(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func refsOf(rs []resourceTouch, intent string) []string {
	var out []string
	for _, r := range rs {
		if r.Intent == intent {
			out = append(out, r.Ref)
		}
	}
	return out
}

func hasRef(rs []resourceTouch, ref, intent string) bool {
	for _, r := range rs {
		if r.Ref == ref && r.Intent == intent {
			return true
		}
	}
	return false
}

func TestResources_LargeWriteKeepsFileRefWhileSummaryLosesIt(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	target := filepath.Join(main, "pkg", "big.go")
	content := strings.Repeat("x", 10*1024)
	withStdin(t, string(toolInput(t, map[string]any{
		"session_id": "s1", "cwd": main, "tool_name": "Write", "tool_use_id": "tu1",
		"tool_input": map[string]any{"file_path": target, "content": content},
	})), cmdNbusActivity)
	evs := get()
	if len(evs) != 1 || evs[0].channel != chanAgentActivity {
		t.Fatalf("want one activity event, got %+v", evs)
	}
	p := evs[0].payload
	if strings.Contains(p["tool_summary"].(string), "file_path") {
		t.Fatalf("precondition: truncated summary unexpectedly kept file_path (defect fixed elsewhere?)")
	}
	raw, _ := json.Marshal(p["resources"])
	var rs []resourceTouch
	_ = json.Unmarshal(raw, &rs)
	if !hasRef(rs, "file:"+target, "write") || !hasRef(rs, "repo:myrepo/pkg/big.go", "write") {
		t.Fatalf("resources = %v", rs)
	}
}

func TestResources_MultiEditAndNotebookEdit(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	p := filepath.Join(main, "a.go")
	rs := extractResources("MultiEdit", main, toolInput(t, map[string]any{
		"file_path": p, "edits": []any{map[string]any{"old_string": "a", "new_string": "b"}},
	}))
	if !hasRef(rs, "file:"+p, "write") {
		t.Fatalf("MultiEdit: %v", rs)
	}
	nb := filepath.Join(main, "n.ipynb")
	rs = extractResources("NotebookEdit", main, toolInput(t, map[string]any{"notebook_path": nb, "new_source": "x"}))
	if !hasRef(rs, "file:"+nb, "write") || !hasRef(rs, "repo:myrepo/n.ipynb", "write") {
		t.Fatalf("NotebookEdit: %v", rs)
	}
}

func TestResources_TwoWorktreesShareLogicalRef(t *testing.T) {
	capturePublish(t)
	_, wts := repoFixture(t, "shared", "lane-a", "lane-b")
	var repoRefs, fileRefs []string
	for _, wt := range []string{"lane-a", "lane-b"} {
		p := filepath.Join(wts[wt], "internal", "x.go")
		rs := extractResources("Edit", wts[wt], toolInput(t, map[string]any{"file_path": p}))
		repoRefs = append(repoRefs, refsOf(rs, "write")[1])
		fileRefs = append(fileRefs, refsOf(rs, "write")[0])
	}
	if repoRefs[0] != "repo:shared/internal/x.go" || repoRefs[0] != repoRefs[1] {
		t.Fatalf("logical refs differ: %v", repoRefs)
	}
	if fileRefs[0] == fileRefs[1] {
		t.Fatalf("physical refs must differ: %v", fileRefs)
	}
}

func TestResources_NotInRepoAndGitInternals(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	outside := filepath.Join(t.TempDir(), "loose.txt")
	rs := extractResources("Write", "/", toolInput(t, map[string]any{"file_path": outside}))
	if len(rs) != 1 || !strings.HasPrefix(rs[0].Ref, "file:") {
		t.Fatalf("outside repo: %v", rs)
	}
	rs = extractResources("Edit", main, toolInput(t, map[string]any{"file_path": filepath.Join(main, ".git", "config")}))
	if len(rs) != 1 {
		t.Fatalf(".git internals must not get a repo ref: %v", rs)
	}
}

func TestResources_RelativeAndTraversalPaths(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	rs := extractResources("Read", filepath.Join(main, "a", "b"), toolInput(t, map[string]any{"file_path": "../c.go"}))
	if !hasRef(rs, "repo:myrepo/a/c.go", "read") {
		t.Fatalf("relative path not resolved against cwd: %v", rs)
	}
	rs = extractResources("Read", filepath.Join(main, "a"), toolInput(t, map[string]any{"file_path": "../../../../../etc/passwd"}))
	for _, r := range rs {
		if strings.HasPrefix(r.Ref, "repo:") {
			t.Fatalf("escaping path must not map into the repo: %v", rs)
		}
	}
	if got := extractResources("Read", "", toolInput(t, map[string]any{"file_path": "rel.go"})); got != nil {
		t.Fatalf("relative path with unknown cwd must yield nothing: %v", got)
	}
}

func TestResources_GrepGlobEmitAsReads(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	payload := func(tool string, in map[string]any) string {
		return string(toolInput(t, map[string]any{
			"session_id": "s", "cwd": main, "tool_name": tool, "tool_use_id": "t-" + tool,
			"tool_input":    in,
			"tool_response": map[string]any{"matches": "SECRET-MATCH-BODY"},
		}))
	}
	withStdin(t, payload("Grep", map[string]any{"pattern": "foo", "path": filepath.Join(main, "src")}), cmdNbusActivity)
	withStdin(t, payload("Glob", map[string]any{"pattern": "internal/**/*.go"}), cmdNbusActivity)
	evs := get()
	if len(evs) != 2 {
		t.Fatalf("Grep and Glob must publish; got %d events", len(evs))
	}
	for _, e := range evs {
		if strings.Contains(e.raw, "SECRET-MATCH-BODY") {
			t.Fatalf("search output leaked: %s", e.raw)
		}
		if _, ok := e.payload["tool_response_summary"]; ok {
			t.Fatalf("search events must not carry tool_response_summary")
		}
	}
	g, _ := json.Marshal(evs[0].payload["resources"])
	if !strings.Contains(string(g), "repo:myrepo/src") {
		t.Fatalf("Grep resources: %s", g)
	}
	g, _ = json.Marshal(evs[1].payload["resources"])
	if !strings.Contains(string(g), "repo:myrepo/internal") || strings.Contains(string(g), "*") {
		t.Fatalf("Glob root must be the literal prefix: %s", g)
	}
}

func TestResources_BashNonAllowlistedYieldsNoPathRefs(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	for _, cmd := range []string{
		"curl -s https://example.com/etc/passwd -o /tmp/out.txt",
		"rm -rf /tmp/some/dir && cat /etc/hosts > /home/x/y",
		"echo hi | tee " + filepath.Join(main, "f.txt"),
		"sed -i s/a/b/ " + filepath.Join(main, "f.txt"),
	} {
		rs := extractResources("Bash", main, toolInput(t, map[string]any{"command": cmd}))
		for _, r := range rs {
			if !strings.HasPrefix(r.Ref, "cmd:") || r.Intent != "exec" {
				t.Errorf("%q produced non-verb ref %v", cmd, r)
			}
		}
		if len(rs) == 0 {
			t.Errorf("%q: expected a verb ref", cmd)
		}
	}
}

func TestResources_BashAllowlistedPaths(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	other, _ := repoFixture(t, "otherrepo")
	cases := []struct {
		cmd  string
		want []string
	}{
		{"git -C " + other + " status", []string{"file:" + other, "repo:otherrepo/"}},
		{"make -C " + other + " build", []string{"file:" + other}},
		{"go test ./internal/foo/...", []string{"file:" + filepath.Join(main, "internal", "foo")}},
		{"go test -run TestX -count 1 ./pkg/a ./pkg/b", []string{"file:" + filepath.Join(main, "pkg", "a"), "file:" + filepath.Join(main, "pkg", "b")}},
		{"cd " + other + " && go test ./...", []string{"file:" + other}},
		{"pytest tests/test_a.py::test_x -q", []string{"file:" + filepath.Join(main, "tests", "test_a.py")}},
		{"python3 -m pytest -k expr tests/", []string{"file:" + filepath.Join(main, "tests")}},
	}
	for _, c := range cases {
		rs := extractResources("Bash", main, toolInput(t, map[string]any{"command": c.cmd}))
		for _, w := range c.want {
			if !hasRef(rs, w, "exec") {
				t.Errorf("%q: missing %s in %v", c.cmd, w, rs)
			}
		}
	}
}

func TestResources_BashDynamicOrAmbiguousPathsNeverGuessed(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	for _, cmd := range []string{
		`git -C "$DIR" status`,
		"git -C $(pwd) log",
		"git -C ~/proj status",
		"make -C ${X} all",
		"go test $PKG",
		"go test -run ./foo ./real",       // value of -run is not a path
		"go test github.com/x/y/pkg",      // import path, not a directory
		"pytest $(ls tests)",              // command substitution
		"pytest -k tests/foo.py",          // value of -k is not a path
		"pytest --rootdir /elsewhere -q",  // unknown value flag swallows its arg
		"cd $X && go test ./...",          // unknown cwd: relative pkg unresolved
		"(cd /somewhere && go test ./..)", // subshell is not parsed
	} {
		rs := extractResources("Bash", main, toolInput(t, map[string]any{"command": cmd}))
		for _, r := range rs {
			if strings.HasPrefix(r.Ref, "file:") || strings.HasPrefix(r.Ref, "repo:") {
				if cmd == "go test -run ./foo ./real" && strings.HasSuffix(r.Ref, "/real") {
					continue
				}
				t.Errorf("%q guessed a path: %v", cmd, r)
			}
		}
	}
}

func TestResources_BashHeredocBodyIsNotACommand(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	cmd := "cat > notes.txt <<'EOF'\ngit -C /etc/shadow status\nmake -C /root all\nEOF"
	rs := extractResources("Bash", main, toolInput(t, map[string]any{"command": cmd}))
	for _, r := range rs {
		if strings.HasPrefix(r.Ref, "file:") || strings.Contains(r.Ref, "shadow") {
			t.Fatalf("heredoc body leaked a path: %v", rs)
		}
	}
}

func TestResources_CapAndDedupe(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	var args []string
	for i := 0; i < 40; i++ {
		args = append(args, fmt.Sprintf("./p%d", i))
	}
	rs := extractResources("Bash", main, toolInput(t, map[string]any{"command": "go test " + strings.Join(args, " ")}))
	if len(rs) > maxResourcesPerEvent {
		t.Fatalf("resources not capped: %d", len(rs))
	}
}

// Attack: file contents and prompt bodies must never reach the bus through
// the fields this change adds.
func TestResources_NoFileContentInEvent(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	const secret = "AKIAIOSFODNN7EXAMPLE-sk-live-9f8e7d6c5b4a"
	big := strings.Repeat("A", 5000) + secret + strings.Repeat("B", 5000)
	withStdin(t, string(toolInput(t, map[string]any{
		"session_id": "s", "cwd": main, "tool_name": "Write", "tool_use_id": "t1",
		"tool_input": map[string]any{"file_path": filepath.Join(main, "creds.env"), "content": big},
	})), cmdNbusActivity)
	for _, e := range get() {
		if strings.Contains(e.raw, secret) {
			t.Fatalf("file content reached the event: %.300s", e.raw)
		}
	}
	// New fields alone, even for content short enough to sit inside the
	// pre-existing tool_summary window.
	rs := extractResources("Write", main, toolInput(t, map[string]any{
		"file_path": filepath.Join(main, "creds.env"), "content": secret,
	}))
	b, _ := json.Marshal(rs)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "sk-live") {
		t.Fatalf("resources carry content: %s", b)
	}
	for _, r := range rs {
		if r.Intent == "" || !strings.Contains(r.Ref, ":") {
			t.Fatalf("malformed touch %v", r)
		}
	}
}

func TestResources_RepoCacheAvoidsRewalk(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	deep := filepath.Join(main, "a", "b", "c", "d")
	_, r1 := refsForPath(filepath.Join(deep, "x.go"))
	// Remove the marker: a cache hit must still answer (no second walk).
	if err := os.Rename(filepath.Join(main, ".git"), filepath.Join(main, ".git.gone")); err != nil {
		t.Fatal(err)
	}
	_, r2 := refsForPath(filepath.Join(deep, "y.go"))
	if r1 == "" || r2 != "repo:myrepo/a/b/c/d/y.go" {
		t.Fatalf("cache miss on second lookup: %q %q", r1, r2)
	}
}

func TestResources_DispatchToolsExecOnly(t *testing.T) {
	capturePublish(t)
	for _, tool := range []string{"Agent", "Task", "Workflow"} {
		rs := extractResources(tool, "/x", toolInput(t, map[string]any{"prompt": "do things in /etc"}))
		if len(rs) != 1 || rs[0].Intent != "exec" || !strings.HasPrefix(rs[0].Ref, "cmd:") {
			t.Fatalf("%s: %v", tool, rs)
		}
	}
	if got := extractResources("WebFetch", "/x", toolInput(t, map[string]any{"url": "http://x"})); got != nil {
		t.Fatalf("unknown tool must yield nil: %v", got)
	}
}

// ------------------------------------------------------------------- scope

func TestScope_AbsentIsNull(t *testing.T) {
	f := buildAssignment("Agent", "toolu_1", "sess", toolInput(t, map[string]any{
		"description": "do it", "prompt": "Fix the thing.\nFILE SCOPE (exclusive): internal/x\nscope: be careful",
	}))
	b, _ := json.Marshal(f)
	if !strings.Contains(string(b), `"scope":null`) {
		t.Fatalf("absent scope must be explicit null: %s", b)
	}
}

func TestScope_ValidLine(t *testing.T) {
	sb := extractScope("intro\nscope: write=internal/a/**,internal/b.go; read=docs/**\nrest")
	want := &scopeBlock{Write: []string{"internal/a/**", "internal/b.go"}, ReadHint: []string{"docs/**"}}
	if !reflect.DeepEqual(sb, want) {
		t.Fatalf("got %+v", sb)
	}
	if sb := extractScope("`scope: read=a/**`"); sb == nil || len(sb.ReadHint) != 1 || sb.Write != nil {
		t.Fatalf("read-only scope: %+v", sb)
	}
}

func TestScope_MalformedIsNullNotPartial(t *testing.T) {
	for _, bad := range []string{
		"scope: write=a/**; read=",                 // empty list
		"scope: write=a/** read=b/**",              // missing separator -> glob contains space
		"scope: write=a/**; write=b/**",            // duplicate key
		"scope: write=a/**; exec=b/**",             // unknown key
		"scope: write=a/**,,b/**",                  // empty element
		"scope: write",                             // no value (not scope-intended: ignored)
		"scope: write=a b/**",                      // whitespace in glob
		"scope: write=a/**; read=b/**; extra",      // trailing junk clause
		"scope: write=" + strings.Repeat("x", 300), // oversize glob
		"scope: write=a/**\nscope: write=b/**",     // ambiguous: two declarations
		"scope: write=a/**\nscope: read=b/** junk", // one valid + one malformed
	} {
		if sb := extractScope("x\n" + bad + "\ny"); sb != nil {
			t.Errorf("%q parsed to %+v; want nil", bad, sb)
		}
	}
}

func TestScope_AbsolutePathsMoveToRefSpace(t *testing.T) {
	capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	sb := extractScope("scope: write=" + main + "/internal/**")
	if sb == nil || len(sb.Write) != 1 || sb.Write[0] != "repo:myrepo/internal/**" {
		t.Fatalf("got %+v", sb)
	}
}

func TestAssignment_RefsConservative(t *testing.T) {
	got := extractAssignmentRefs("See deer-flow#121, bd-ab12 and https://github.com/o/tachyonac-engine/issues/559; bare #5, v#7 and 12#3 are not refs.")
	for _, w := range []string{"bead:bd-ab12", "issue:tachyonac-engine#559", "issue:deer-flow#121"} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("missing %s in %v", w, got)
		}
	}
	for _, r := range got {
		if strings.Contains(r, "#5") && !strings.Contains(r, "559") || strings.HasPrefix(r, "issue:v#") || strings.Contains(r, "12#") {
			t.Fatalf("non-ref accepted: %v", got)
		}
	}
}

func TestAssignment_EndToEndPublish(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	const secret = "PROMPT-BODY-SECRET-ghp_abcdef1234567890"
	withStdin(t, string(toolInput(t, map[string]any{
		"session_id": "sess-1", "cwd": main, "tool_name": "Agent", "tool_use_id": "toolu_42",
		"tool_input": map[string]any{
			"description": "Implement lane O1a", "subagent_type": "sonnet-worker",
			"prompt": "Work on hearth#307.\nscope: write=tools/claude-hooks/**\n" + secret,
		},
	})), cmdNbusAssignment)
	evs := get()
	if len(evs) != 1 || evs[0].channel != chanWorkAssignment {
		t.Fatalf("events: %+v", evs)
	}
	p := evs[0].payload
	if p["assignment_id"] != "toolu_42" || p["assigned_by"] != "sess-1" || p["title"] != "Implement lane O1a" {
		t.Fatalf("fields: %v", p)
	}
	w := p["worker"].(map[string]any)
	if w["kind"] != "claude-subagent" || w["id"] != "toolu_42" {
		t.Fatalf("worker: %v", w)
	}
	sc := p["scope"].(map[string]any)
	if !reflect.DeepEqual(sc["write"], []any{"tools/claude-hooks/**"}) {
		t.Fatalf("scope: %v", sc)
	}
	if !reflect.DeepEqual(p["refs"], []any{"issue:hearth#307"}) {
		t.Fatalf("refs: %v", p["refs"])
	}
	if strings.Contains(evs[0].raw, secret) {
		t.Fatalf("prompt body leaked: %s", evs[0].raw)
	}
}

func TestAssignment_NullScopeSurvivesPublish(t *testing.T) {
	get := capturePublish(t)
	withStdin(t, string(toolInput(t, map[string]any{
		"session_id": "s", "cwd": "/", "tool_name": "Task", "tool_use_id": "toolu_9",
		"tool_input": map[string]any{"description": "d", "prompt": "no scope here"},
	})), cmdNbusAssignment)
	evs := get()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	v, present := evs[0].payload["scope"]
	if !present || v != nil {
		t.Fatalf("scope must be present and null on the wire: present=%v v=%v raw=%s", present, v, evs[0].raw)
	}
}

func TestAssignment_NoToolUseIDOrWrongToolPublishesNothing(t *testing.T) {
	get := capturePublish(t)
	withStdin(t, `{"session_id":"s","cwd":"/","tool_name":"Agent","tool_input":{"description":"d"}}`, cmdNbusAssignment)
	withStdin(t, `{"session_id":"s","cwd":"/","tool_name":"Bash","tool_use_id":"t","tool_input":{"command":"ls"}}`, cmdNbusAssignment)
	if n := len(get()); n != 0 {
		t.Fatalf("published %d events; want 0", n)
	}
}

// -------------------------------------------------------------- benchmarks

// BenchmarkActivityEnvelope measures the in-process cost of building an
// activity event. "baseline" is the pre-existing work (summaries only);
// "with_resources" adds resource extraction; "cold" additionally clears the
// repo cache each iteration, which is the real per-call cost because the hook
// is a fresh process every time.
func BenchmarkActivityEnvelope(b *testing.B) {
	root := b.TempDir()
	main := filepath.Join(root, "repos", "myrepo")
	_ = os.MkdirAll(filepath.Join(main, ".git", "worktrees", "w"), 0o755)
	_ = os.WriteFile(filepath.Join(main, ".git", "worktrees", "w", "commondir"), []byte("../..\n"), 0o644)
	wt := filepath.Join(root, "wts", "w")
	_ = os.MkdirAll(filepath.Join(wt, "internal", "deep", "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "w")+"\n"), 0o644)
	file := filepath.Join(wt, "internal", "deep", "pkg", "x.go")

	mk := func(tool string, in map[string]any) event.HookInput {
		raw, _ := json.Marshal(in)
		return event.HookInput{SessionID: "s", CWD: wt, ToolName: tool, ToolUseID: "t", ToolInput: raw, ToolResponse: json.RawMessage(`{"ok":true}`)}
	}
	cases := map[string]event.HookInput{
		"Write10KB": mk("Write", map[string]any{"file_path": file, "content": strings.Repeat("x", 10*1024)}),
		"Read":      mk("Read", map[string]any{"file_path": file}),
		"Bash":      mk("Bash", map[string]any{"command": "cd " + wt + " && go test ./internal/... -run TestX"}),
	}
	for name, in := range cases {
		b.Run(name+"/baseline", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				env, _ := baseEnvelope(in, "tool_call", in.AgentKind())
				env["tool_name"] = in.ToolName
				env["tool_summary"] = boundToolSummary(in.ToolName, in.ToolInput)
				env["tool_response_summary"] = boundedToolResponseSummary(in.ToolResponse)
			}
		})
		b.Run(name+"/with_resources_warm", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = buildActivityEnvelope(in)
			}
		})
		b.Run(name+"/with_resources_cold", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				resetRepoCache()
				_, _ = buildActivityEnvelope(in)
			}
		})
	}
}
