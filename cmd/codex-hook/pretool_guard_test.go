package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// runPreTool feeds a raw Codex PreToolUse payload to the real "pre-tool"
// cobra command (readPayload() reads os.Stdin; writePreToolBlock() writes
// os.Stdout), and reports whether it blocked plus the deny reason.
//
// The payload shapes below are the exact Codex 0.149 PreToolUse key set this
// binary already models (see payload.go's hookPayload doc comment and
// lifecycle_test.go's TestPayloadDecodePreservesCodex0149Fields), extended
// with tool_input.command the way a shell/exec tool call carries it.
func runPreTool(t *testing.T, rawPayload string) (blocked bool, reason string) {
	t.Helper()

	origStdin, origStdout := os.Stdin, os.Stdout
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() { os.Stdin, os.Stdout = origStdin, origStdout })

	// publishAsync fires from emitHook on every pre-tool call; stub it so
	// tests never touch the real nervous-bus publish path.
	origPublish := publishAsync
	publishAsync = func(string, string, string) error { return nil }
	t.Cleanup(func() { publishAsync = origPublish })

	go func() {
		_, _ = io.WriteString(inW, rawPayload)
		_ = inW.Close()
	}()

	cmd := newCodexHookPreToolCmd()
	runErr := cmd.RunE(cmd, nil)
	_ = outW.Close()
	out, _ := io.ReadAll(outR)
	if runErr != nil {
		t.Fatalf("pre-tool RunE returned error: %v", runErr)
	}
	if len(out) == 0 {
		return false, ""
	}
	var decision preToolDecision
	if err := json.Unmarshal(out, &decision); err != nil {
		t.Fatalf("pre-tool stdout not valid preToolDecision JSON: %v (%s)", err, out)
	}
	return decision.HookSpecificOutput.PermissionDecision == "deny", decision.HookSpecificOutput.PermissionDecisionReason
}

// codexPreToolPayload builds a real-shaped Codex PreToolUse JSON payload
// (Bash tool, tool_input.command) with the given command.
func codexPreToolPayload(sessionID, cwd, command string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"cwd":             cwd,
		"hook_event_name": "PreToolUse",
		"model":           "gpt-6-astra",
		"permission_mode": "on-request",
		"turn_id":         "turn-1",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": command},
		"tool_use_id":     "tool-1",
	})
	return string(b)
}

func TestPreToolShellGuards_RealCodexPayloadShapes(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		wantBlocked bool
		wantReason  string // substring
	}{
		{"sudo blocked", "sudo apt-get update", true, "sudo"},
		{"bare git stash blocked", "git stash", true, "WHOLE-TREE git stash"},
		{"git stash pop blocked", "git stash pop", true, "git stash pop"},
		{"git clean -f blocked", "git clean -fd", true, "git clean -f"},
		{"git reset hard origin blocked", "git reset --hard origin/main", true, "git reset --hard"},
		{"rm -rf outside scratch blocked", "rm -rf /home/eric/data2/worktrees/foo/build", true, "recursive delete"},
		{"ordinary git commit passes", "git add -A && git commit -m fix", false, ""},
		{"ordinary read passes", "cat README.md", false, ""},
		{"rm -rf under evidence passes", "rm -rf /home/eric/data2/orca/proj/_evidence/2026-09-25/scratch", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := codexPreToolPayload("sess-1", "/home/eric/data2/worktrees/foo", tt.command)
			blocked, reason := runPreTool(t, payload)
			if blocked != tt.wantBlocked {
				t.Fatalf("command %q: blocked=%v reason=%q, want blocked=%v", tt.command, blocked, reason, tt.wantBlocked)
			}
			if tt.wantBlocked && !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("command %q: reason %q does not contain %q", tt.command, reason, tt.wantReason)
			}
		})
	}
}

// TestPreToolNonShellToolPassesThroughFast covers the AC's non-shell-tool
// requirement: with the Codex PreToolUse matcher now "" (every tool reaches
// the hook, matching ~/.codex/config.toml), apply_patch and MCP tool calls
// carry no "command"/"cmd" key and must pass straight through with no
// decision emitted at all — never blocked, never even a spurious allow
// decision written to stdout.
func TestPreToolNonShellToolPassesThroughFast(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			"apply_patch",
			`{"session_id":"s1","cwd":"/repo","hook_event_name":"PreToolUse","tool_name":"apply_patch","tool_input":{"patch":"*** Begin Patch\n*** End Patch"}}`,
		},
		{
			"mcp tool",
			`{"session_id":"s1","cwd":"/repo","hook_event_name":"PreToolUse","tool_name":"mcp__context7__get-library-docs","tool_input":{"library":"react"}}`,
		},
		{
			"malformed json fails open",
			`{not json`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocked, reason := runPreTool(t, tt.payload)
			if blocked {
				t.Fatalf("non-shell/malformed payload blocked: %q", reason)
			}
		})
	}
}

func TestPreToolFailsOpenOnMalformedJSON(t *testing.T) {
	blocked, _ := runPreTool(t, `{"tool_name": "Bash", "tool_input": {`)
	if blocked {
		t.Fatal("malformed payload must fail open, not block")
	}
}
