package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyVerify(t *testing.T) {
	cases := []struct{ cmd, class, phrase string }{
		{"cargo test -p foo", "test", "cargo test"},
		{"timeout 600 cargo nextest run", "test", "cargo nextest"},
		{"cd x && go test ./... 2>&1 | tail -20", "test", "go test"},
		{"go build ./...", "build", "go build"},
		{"go vet ./...", "lint", "go vet"},
		{"go -C tools test ./...", "test", "go test"},
		{"nice -n 10 python3 -I -m pytest -q tests/", "test", "python -m pytest"},
		{"python3 -m mypy src", "typecheck", "python -m mypy"},
		{"uv run pytest -x", "test", "pytest"},
		{"uv run --with foo pytest", "test", "pytest"},
		{"npx vitest run", "test", "vitest"},
		{"pnpm exec tsc --noEmit", "typecheck", "tsc"},
		{"npm test", "test", "npm test"},
		{"npm run test:unit -- --ci", "test", "npm run test"},
		{"npm run lint -- --fix", "lint", "npm run lint"},
		{"yarn build", "build", "yarn run build"},
		{"pnpm --filter web run typecheck", "typecheck", "pnpm run typecheck"},
		{"make -C tools test", "test", "make test"},
		{"make -j 8 build", "build", "make build"},
		{"sudo -u eric cargo test", "test", "cargo test"},
		{"./run_tests.sh", "test", "test script"},
		{"bash scripts/fast-gate.sh", "test", "test script"},
		{"python3 -I tools/test_flow.py", "test", "test script"},
		{"cargo build && cargo test", "test", "cargo test"},
		{"cmake --build build -j8", "build", "cmake --build"},
		// Not verification.
		{"make install", "", ""},
		{"git status", "", ""},
		{"echo 'cargo test'", "", ""},
		{"rg 'cargo test' src", "", ""},
		{"cat <<'EOF' > x.sh\ncargo test\nEOF", "", ""},
		{"FOO=$(cat k) cargo test", "", ""},
		{"npm install", "", ""},
		{"python3 script.py", "", ""},
	}
	for _, c := range cases {
		class, phrase := classifyVerify(c.cmd)
		if class != c.class || phrase != c.phrase {
			t.Errorf("classifyVerify(%q) = (%q, %q), want (%q, %q)", c.cmd, class, phrase, c.class, c.phrase)
		}
	}
}

// The phrase comes from fixed tables: a script or target name never reaches it.
func TestClassifyVerify_PhraseNeverCarriesArgs(t *testing.T) {
	for _, cmd := range []string{
		"npm run test:SECRET-9f9f",
		"yarn build-SECRET-9f9f",
		"make test SECRET-9f9f=1",
		"cargo test SECRET-9f9f",
		"python3 -m pytest -k SECRET-9f9f",
	} {
		class, phrase := classifyVerify(cmd)
		if class == "" {
			t.Fatalf("precondition: %q should classify", cmd)
		}
		if strings.Contains(phrase, "SECRET") {
			t.Errorf("phrase %q leaks args of %q", phrase, cmd)
		}
	}
}

func TestBashVerifyOutcome(t *testing.T) {
	resp := func(stdout string, extra map[string]any) json.RawMessage {
		m := map[string]any{"stdout": stdout, "stderr": ""}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	cases := []struct {
		name, cmd string
		raw       json.RawMessage
		want      string
	}{
		{"cargo pass", "cargo test", resp("test result: ok. 5 passed; 0 failed; 0 ignored", nil), "pass"},
		{"cargo fail behind tail", "cargo test 2>&1 | tail -3", resp("test result: FAILED. 4 passed; 1 failed", nil), "fail"},
		{"go fail behind tail", "go test ./... | tail", resp("--- FAIL: TestX (0.00s)\nFAIL\nok  \tother\t0.1s", nil), "fail"},
		{"pytest pass", "python3 -m pytest -q", resp("=== 3 passed in 0.50s ===", nil), "pass"},
		{"pytest fail", "pytest -q | tail -1", resp("=== 1 failed, 2 passed in 0.50s ===", nil), "fail"},
		{"vitest fail", "npx vitest run", resp(" FAIL  src/a.test.ts > adds\nTests  1 failed | 4 passed", nil), "fail"},
		{"tsc fail", "npx tsc --noEmit | head", resp("src/a.ts(3,1): error TS2304: Cannot find name 'x'.", nil), "fail"},
		{"background launch", "cargo test", resp("", map[string]any{"backgroundTaskId": "b1"}), "unknown"},
		{"interrupted", "cargo test", resp("running 5 tests", map[string]any{"interrupted": true}), "unknown"},
		{"not a verify", "ls -la", resp("1 failed", nil), ""},
	}
	for _, c := range cases {
		got, _, _ := bashVerifyOutcome(c.cmd, c.raw)
		if got != c.want {
			t.Errorf("%s: bashVerifyOutcome = %q, want %q", c.name, got, c.want)
		}
	}
}

// A failed Bash verify reaches the bus as an error with its exit code and a
// fail outcome. The failure text, which carries the command output, does not.
func TestNbusActivity_PostToolUseFailure(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	const secret = "SECRET-FAILOUT-4e4e"
	payload := map[string]any{
		"hook_event_name": "PostToolUseFailure",
		"session_id":      "s", "cwd": main, "tool_name": "Bash", "tool_use_id": "tu",
		"tool_input":   map[string]any{"command": "cargo test -p core 2>&1 | tail -40"},
		"error":        "Exit code 101\n" + secret + "\ntest result: FAILED. 1 failed",
		"is_interrupt": false,
		"duration_ms":  4321,
	}
	withStdin(t, string(toolInput(t, payload)), cmdNbusActivity)
	evs := get()
	if len(evs) != 1 {
		t.Fatalf("published %d events, want 1", len(evs))
	}
	e := evs[0]
	if strings.Contains(e.raw, secret) {
		t.Fatalf("failure text reached the bus: %.400s", e.raw)
	}
	if e.payload["tool_is_error"] != true || e.payload["tool_error_type"] != "exit" {
		t.Errorf("tool_is_error/type = %v/%v", e.payload["tool_is_error"], e.payload["tool_error_type"])
	}
	if e.payload["duration_ms"] != float64(4321) {
		t.Errorf("duration_ms = %v", e.payload["duration_ms"])
	}
	var ts, rs map[string]any
	if err := json.Unmarshal([]byte(e.payload["tool_summary"].(string)), &ts); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(e.payload["tool_response_summary"].(string)), &rs); err != nil {
		t.Fatal(err)
	}
	if ts["verify_class"] != "test" || ts["verify_cmd"] != "cargo test" {
		t.Errorf("tool_summary = %v", ts)
	}
	if rs["exit_code"] != float64(101) || rs["verify_outcome"] != "fail" || rs["is_error"] != true {
		t.Errorf("tool_response_summary = %v", rs)
	}
}

func TestNbusActivity_InterruptAndNonBashFailure(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	withStdin(t, string(toolInput(t, map[string]any{
		"hook_event_name": "PostToolUseFailure", "session_id": "s", "cwd": main,
		"tool_name": "Bash", "tool_use_id": "tu1", "tool_input": map[string]any{"command": "go test ./..."},
		"error": "Interrupted by user", "is_interrupt": true,
	})), cmdNbusActivity)
	withStdin(t, string(toolInput(t, map[string]any{
		"hook_event_name": "PostToolUseFailure", "session_id": "s", "cwd": main,
		"tool_name": "Edit", "tool_use_id": "tu2",
		"tool_input": map[string]any{"file_path": main + "/a.go", "old_string": "x", "new_string": "y"},
		"error":      "String to replace not found in file.",
	})), cmdNbusActivity)
	evs := get()
	if len(evs) != 2 {
		t.Fatalf("published %d events, want 2", len(evs))
	}
	if evs[0].payload["tool_error_type"] != "interrupt" || !strings.Contains(evs[0].payload["tool_response_summary"].(string), `"verify_outcome":"unknown"`) {
		t.Errorf("interrupt event = %v", evs[0].payload)
	}
	if evs[1].payload["tool_error_type"] != "error" || evs[1].payload["tool_is_error"] != true {
		t.Errorf("edit failure event = %v", evs[1].payload)
	}
}

func TestBashGitOps(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{`cd x && git add a.go && git commit -m "SECRET msg" && git push -u origin b`, "git add git commit git push"},
		{"git -C /home/eric/projects/r commit -F - <<'EOF'\nbody\nEOF", "git commit"},
		{"git -c user.name=x checkout -- src/a.rs", "git checkout"},
		{"timeout 60 git push -q", "git push"},
		{"git status; git status --short", "git status"},
		{"git frobnicate SECRET", ""},
		{"echo git commit", ""},
		{"gh pr create", ""},
	}
	for _, c := range cases {
		if got := bashGitOps(c.cmd); got != c.want {
			t.Errorf("bashGitOps(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
	raw, _ := json.Marshal(map[string]any{"command": `git commit -m "SECRET-GIT-7c7c"`})
	s := boundToolSummary("Bash", raw)
	if strings.Contains(s, "SECRET") || !strings.Contains(s, `"git_ops":"git commit"`) {
		t.Errorf("tool_summary = %s", s)
	}
}

func TestToolSummary_SkillName(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"skill": "code-review:code-review", "args": "SECRET-ARGS-1a1a"})
	s := boundToolSummary("Skill", raw)
	if strings.Contains(s, "SECRET") {
		t.Fatalf("skill args leaked: %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	if m["skill"] != "code-review:code-review" || m["args_bytes"] != float64(16) {
		t.Errorf("summary = %v", m)
	}
	raw, _ = json.Marshal(map[string]any{"skill": "bad name with spaces"})
	if strings.Contains(boundToolSummary("Skill", raw), "bad name") {
		t.Errorf("unsafe skill name emitted")
	}
}
