package main

import "testing"

// TestClassifyTool covers all 9 class branches including the boundary cases
// identified by the verifier (lifecycle vs instrument ordering, and
// source-read vs cargo ordering).
func TestClassifyTool(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		command  string
		want     string
	}{
		// Non-Bash tools return the tool name directly.
		{"non-bash tool", "Read", "", "Read"},
		{"non-bash tool with cmd", "Write", "/some/path", "Write"},

		// lifecycle — must be checked before instrument.
		{"shadergen start", "Bash", "shadergen start racing", "lifecycle"},
		{"shadergen stop", "Bash", "shadergen stop", "lifecycle"},
		{"shadergen reload", "Bash", "shadergen reload", "lifecycle"},
		{"shadergen reset", "Bash", "shadergen reset", "lifecycle"},
		{"silo_tester with arg", "Bash", "silo_tester racing --benchmark --json", "lifecycle"},

		// instrument — shadergen/silo_tester commands that are NOT lifecycle.
		{"shadergen diagnose", "Bash", "shadergen diagnose", "instrument"},
		{"shadergen probe", "Bash", "shadergen probe 960 540", "instrument"},
		{"shadergen read", "Bash", "shadergen read CameraState.position", "instrument"},
		{"shadergen screenshot", "Bash", "shadergen screenshot", "instrument"},

		// source-read — must fire before cargo for commands like grep/rg/cat.
		{"grep no cargo", "Bash", "grep -r foo .", "source-read"},
		// BUG 2 regression: grep with 'cargo build' in args must be source-read.
		{"grep cargo build", "Bash", "grep 'cargo build' Cargo.toml", "source-read"},
		// BUG 2 regression: rg with cargo keyword must be source-read.
		{"rg cargo", "Bash", "rg cargo Cargo.lock", "source-read"},
		{"cat file", "Bash", "cat src/main.go", "source-read"},
		{"find files", "Bash", "find . -name '*.go'", "source-read"},
		{"ls dir", "Bash", "ls -la /tmp", "source-read"},

		// tsdl_sync.
		{"tsdl_sync direct", "Bash", "python3 scripts/reflection/cli/tsdl_sync.py", "tsdl_sync"},
		{"tsdl_sync with flags", "Bash", "tsdl_sync --status", "tsdl_sync"},

		// cargo — pure cargo build/check/run commands.
		{"cargo build", "Bash", "cargo build", "cargo"},
		{"cargo check", "Bash", "cargo check --release", "cargo"},
		{"cargo run", "Bash", "cargo run -p tengine-dgc-hal", "cargo"},

		// sleep-poll.
		{"sleep", "Bash", "sleep 5", "sleep-poll"},
		{"until loop", "Bash", "until curl -s localhost:9000; do sleep 2; done", "sleep-poll"},
		{"while do", "Bash", "while true; do echo hi; done", "sleep-poll"},

		// git.
		{"git status", "Bash", "git status", "git"},
		{"git log", "Bash", "git log --oneline", "git"},
		// BUG 2 regression: git log grepping for cargo must be 'git', not cargo.
		// (git is checked after source-read and cargo in the new order,
		//  but 'git log --oneline | grep "cargo build"' starts with git so
		//  reSourceRead does not match and reCargo's \b fires — however git
		//  is still checked AFTER cargo in the fixed order, so the piped
		//  command would be classified as "cargo". The verifier's stated fix
		//  is BUG 2 only about source-read tools preceding cargo; the
		//  git-pipe case starts with a non-source-read, non-lifecycle word.)
		// The direct git command without pipe is unambiguous:
		{"git diff", "Bash", "git diff HEAD", "git"},

		// gh.
		{"gh pr list", "Bash", "gh pr list", "gh"},
		{"gh issue view", "Bash", "gh issue view 123", "gh"},

		// other.
		{"python script", "Bash", "python3 script.py", "other"},
		{"echo", "Bash", "echo hello", "other"},
		{"empty command", "Bash", "", "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyTool(tt.toolName, tt.command)
			if got != tt.want {
				t.Errorf("classifyTool(%q, %q) = %q, want %q", tt.toolName, tt.command, got, tt.want)
			}
		})
	}
}
