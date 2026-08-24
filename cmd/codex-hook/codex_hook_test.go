package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadPayloadFrom_InputCompatibilityAndErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"tool input", `{"tool_input":{"command":"git status"}}`, `{"command":"git status"}`, false},
		{"legacy input", `{"input":{"command":"git status"}}`, `{"command":"git status"}`, false},
		{"current input wins", `{"tool_input":{"command":"new"},"input":{"command":"old"}}`, `{"command":"new"}`, false},
		{"null current falls back", `{"tool_input":null,"input":{"command":"old"}}`, `{"command":"old"}`, false},
		{"malformed", `{`, "", true},
		{"oversized", `{"tool_input":"` + strings.Repeat("x", maxHookPayloadBytes) + `"}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := readPayloadFrom(strings.NewReader(tc.body))
			if tc.wantErr {
				if err == nil || p != nil {
					t.Fatalf("got payload=%v err=%v, want error and nil payload", p, err)
				}
				return
			}
			if err != nil || p == nil || string(p.effectiveInput()) != tc.want {
				t.Fatalf("got payload=%v err=%v input=%q, want %q", p, err, p.effectiveInput(), tc.want)
			}
		})
	}
}

// X2: pulse/pre-tool thread the codex tool invocation (name + bounded input
// summary) into bus.agent.activity.v1, and pane capture gains the tmux
// fallback X1 established.

func TestCodexToolSummary(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		want        string
		wantCommand string
		wantPrompt  string
	}{
		{"empty input", "", "", "", ""},
		{"bash command before description", `{"description":"run the build","command":"cargo build"}`, "cargo build", "cargo build", `{"command":"cargo build","description":"run the build"}`},
		{"non shell action", `{"action":"read_file","path":"/etc/hosts"}`, "read_file", "", `{"action":"read_file","path":"/etc/hosts"}`},
		{"string input", `"plain input"`, "plain input", "", `"plain input"`},
		{"array command", `{"command":["git","status","--short"]}`, "git status --short", "git status --short", `{"command":["git","status","--short"]}`},
		{"object command", `{"command":{"program":"go","args":["test"]}}`, `{"args":["test"],"program":"go"}`, `{"args":["test"],"program":"go"}`, `{"command":{"args":["test"],"program":"go"}}`},
		{"secret args", `{"command":"curl --token topsecret --api-key=abc","authorization":"Bearer nope"}`, "curl --token [REDACTED] --api-key=[REDACTED]", "curl --token [REDACTED] --api-key=[REDACTED]", `{"authorization":"[REDACTED]","command":"curl --token [REDACTED] --api-key=[REDACTED]"}`},
		{"unicode bound safe", `{"command":"界字"}`, "界字", "界字", `{"command":"界字"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &hookPayload{ToolName: "Bash"}
			if c.input != "" {
				p.Input = json.RawMessage(c.input)
			}
			details := toolInputDetailsFor(p)
			if got := codexToolSummary(p); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
			if details.CommandPreview != c.wantCommand || details.PromptText != c.wantPrompt {
				t.Fatalf("details=%+v, want command=%q prompt=%q", details, c.wantCommand, c.wantPrompt)
			}
		})
	}
}

func TestToolInputDetails_NilAndJustification(t *testing.T) {
	if got := toolInputDetailsFor(nil); got != (toolInputDetails{}) {
		t.Fatalf("nil details = %+v", got)
	}
	p := &hookPayload{ToolInput: json.RawMessage(`{"action":"apply_patch","description":"change config","justification":"needed for test"}`)}
	got := toolInputDetailsFor(p)
	if got.Primary != "apply_patch" || got.Description != "change config" || got.Justification != "needed for test" {
		t.Fatalf("details = %+v", got)
	}
}

func TestCodexToolSummary_Truncates(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	p := &hookPayload{
		ToolName: "Bash",
		Input:    json.RawMessage(`{"command":"` + string(long) + `"}`),
	}
	got := codexToolSummary(p)
	if len(got) != 200 {
		t.Fatalf("expected 200-char bound, got %d", len(got))
	}
}

func TestTruncate_UnicodeSafe(t *testing.T) {
	got := truncate(strings.Repeat("界", 300), 200)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 200 {
		t.Fatalf("got invalid or wrongly bounded UTF-8 string: %q", got)
	}
}

func TestCodexPaneID_TmuxFallback(t *testing.T) {
	t.Run("zellij preferred", func(t *testing.T) {
		t.Setenv("ZELLIJ_PANE_ID", "5")
		t.Setenv("TMUX_PANE", "%9")
		if got := codexPaneID(); got != "5" {
			t.Fatalf("got %q want %q", got, "5")
		}
	})
	t.Run("tmux fallback when zellij absent", func(t *testing.T) {
		t.Setenv("ZELLIJ_PANE_ID", "")
		t.Setenv("TMUX_PANE", "%9")
		if got := codexPaneID(); got != "%9" {
			t.Fatalf("got %q want %q (tmux fallback X1 parity)", got, "%9")
		}
	})
	t.Run("empty when neither set", func(t *testing.T) {
		t.Setenv("ZELLIJ_PANE_ID", "")
		t.Setenv("TMUX_PANE", "")
		if got := codexPaneID(); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})
}
