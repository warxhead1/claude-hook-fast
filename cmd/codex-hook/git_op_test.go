package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// nervous-bus#35 §4 fix 2: a structured git op parsed over the FULL command,
// not the 200-char command_preview truncation.
func TestGitOperationSummary(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		wantCommit   bool
		wantPush     bool
		wantPRCreate bool
		wantPRMerge  bool
		wantEmpty    bool
	}{
		{"plain commit", "git commit -m 'fix bug'", true, false, false, false, false},
		{"plain push", "git push origin main", false, true, false, false, false},
		{"commit then push chained", "git add -A && git commit -m wip && git push", true, true, false, false, false},
		{"git -C worktree commit", "git -C /home/eric/data2/worktrees/foo commit -am fix", true, false, false, false, false},
		{"gh pr create", "gh pr create --title x --body y", false, false, true, false, false},
		{"gh pr merge", "gh pr merge --squash", false, false, false, true, false},
		{
			"long chained command loses nothing (the exact nervous-bus#35 row-1-4 shape)",
			"gofmt -l . && go test ./... && go vet ./... && git add -A && git commit -m 'fix: a very long commit subject describing the change in full detail for context'",
			true, false, false, false, false,
		},
		{"read-only audit, no git op", "cat README.md && grep -rn TODO .", false, false, false, false, true},
		{"diff-tree no-commit-id is NOT a commit (label.py's own false-positive class)", "git diff-tree --no-commit-id --name-only HEAD", false, false, false, false, true},
		{"git log mentioning commit in message text is not a commit call", "git log --oneline --grep='fix commit handling'", false, false, false, false, true},
		{"empty command", "", false, false, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &hookPayload{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":` + jsonQuote(tt.command) + `}`)}
			got := gitOperationSummary(p)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("gitOperationSummary(%q) = %q, want empty", tt.command, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("gitOperationSummary(%q) = empty, want a gitOperation summary", tt.command)
			}
			var parsed map[string]map[string]any
			if err := json.Unmarshal([]byte(got), &parsed); err != nil {
				t.Fatalf("gitOperationSummary(%q) not valid JSON: %v (%s)", tt.command, err, got)
			}
			op, ok := parsed["gitOperation"]
			if !ok {
				t.Fatalf("gitOperationSummary(%q) missing top-level gitOperation key: %s", tt.command, got)
			}
			if _, has := op["commit"]; has != tt.wantCommit {
				t.Errorf("commit present=%v, want %v (%s)", has, tt.wantCommit, got)
			}
			if _, has := op["push"]; has != tt.wantPush {
				t.Errorf("push present=%v, want %v (%s)", has, tt.wantPush, got)
			}
			if _, has := op["pr_create"]; has != tt.wantPRCreate {
				t.Errorf("pr_create present=%v, want %v (%s)", has, tt.wantPRCreate, got)
			}
			if _, has := op["pr_merge"]; has != tt.wantPRMerge {
				t.Errorf("pr_merge present=%v, want %v (%s)", has, tt.wantPRMerge, got)
			}
		})
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nervous-bus#35 §4 fix 3: tool_is_error read from tool_response. Current
// Codex runtime (verified live against openai/codex#34289, open through
// 0.154.0) sends tool_response as a plain string with no failure signal at
// all — every one of those cases must resolve to ok=false (omitted field,
// never a fabricated guess), while a hypothetical future/alternate shape
// carrying is_error or exit_code is read correctly.
func TestCodexToolIsError(t *testing.T) {
	tests := []struct {
		name        string
		toolResp    string
		wantIsError bool
		wantOK      bool
	}{
		{"current codex shape: plain string success", `"ok\n"`, false, false},
		{"current codex shape: plain string empty (quiet failure, indistinguishable)", `""`, false, false},
		{"current codex shape: plain string with error-looking text is NOT inferred as an error", `"cat: /nonexistent-file: No such file or directory\n"`, false, false},
		{"no tool_response at all", ``, false, false},
		{"forward-compat: explicit is_error true", `{"is_error":true,"output":"boom"}`, true, true},
		{"forward-compat: explicit is_error false", `{"is_error":false,"output":"ok"}`, false, true},
		{"forward-compat: exit_code nonzero", `{"output":"boom","exit_code":1}`, true, true},
		{"forward-compat: exit_code zero", `{"output":"ok","exit_code":0}`, false, true},
		{"forward-compat: proposed tool_exit_code field", `{"output":"boom","tool_exit_code":7}`, true, true},
		{"forward-compat: nested status.exit_code", `{"status":{"exit_code":2}}`, true, true},
		{"malformed json", `{not json`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &hookPayload{}
			if tt.toolResp != "" {
				p.ToolResponse = json.RawMessage(tt.toolResp)
			}
			isErr, ok := codexToolIsError(p)
			if ok != tt.wantOK {
				t.Fatalf("codexToolIsError(%s) ok=%v, want %v", tt.toolResp, ok, tt.wantOK)
			}
			if ok && isErr != tt.wantIsError {
				t.Fatalf("codexToolIsError(%s) isError=%v, want %v", tt.toolResp, isErr, tt.wantIsError)
			}
		})
	}
}

// nervous-bus#35 §4 fix 6: heartbeats must not carry tool_name (a stale
// leftover value made every heartbeat look like a tool call to label.py's
// behavior inference).
func TestHeartbeatDoesNotCarryToolName(t *testing.T) {
	p := &hookPayload{SessionID: "s1", ToolName: "Bash"}
	heartbeat, _ := buildEnvelope(p, "heartbeat")
	if _, present := heartbeat["tool_name"]; present {
		t.Fatalf("heartbeat envelope carries tool_name: %v", heartbeat["tool_name"])
	}
	toolCall, _ := buildEnvelope(p, "tool_call")
	if toolCall["tool_name"] != "Bash" {
		t.Fatalf("tool_call envelope lost tool_name: %v", toolCall["tool_name"])
	}
	toolReturn, _ := buildEnvelope(p, "tool_return")
	if toolReturn["tool_name"] != "Bash" {
		t.Fatalf("tool_return envelope lost tool_name: %v", toolReturn["tool_name"])
	}
}

// End-to-end: emitHook's actual PostToolUse path (wire "tool_return") should
// carry both the structured gitOperation (via tool_response_summary) and
// tool_is_error together when both signals are present, and the envelope
// must be valid JSON reflex-recorder's label.py can parse directly.
func TestBuildEnvelope_PostToolUseCarriesGitOpAndError(t *testing.T) {
	p := &hookPayload{
		SessionID:    "s1",
		ToolName:     "Bash",
		ToolInput:    json.RawMessage(`{"command":"git add -A && git commit -m fix"}`),
		ToolResponse: json.RawMessage(`{"is_error":true,"output":"pre-commit hook failed"}`),
	}
	env, _ := buildEnvelope(p, "tool_return")
	summary, _ := env["tool_response_summary"].(string)
	if !strings.Contains(summary, `"gitOperation"`) || !strings.Contains(summary, `"commit"`) {
		t.Fatalf("tool_response_summary missing gitOperation.commit: %q", summary)
	}
	if env["tool_is_error"] != true {
		t.Fatalf("tool_is_error = %v, want true", env["tool_is_error"])
	}
}
