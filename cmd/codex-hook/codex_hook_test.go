package main

import (
	"encoding/json"
	"testing"
)

// X2: pulse/pre-tool thread the codex tool invocation (name + bounded input
// summary) into bus.agent.activity.v1, and pane capture gains the tmux
// fallback X1 established.

func TestCodexToolSummary(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty input", "", ""},
		{"command safely preferred", `{"description":"run the build","command":"cargo build"}`, "cargo build"},
		{"command fallback", `{"command":"cargo build"}`, "cargo build"},
		{"raw json when no known field", `{"path":"/etc/hosts"}`, `{"path":"/etc/hosts"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &hookPayload{ToolName: "Bash"}
			if c.input != "" {
				p.Input = json.RawMessage(c.input)
			}
			if got := codexToolSummary(p); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
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

// nervous-bus#34: inherited AI_AGENT / ANTHROPIC_BASE_URL must not relabel codex.
func TestDetectAgentKind_IgnoresInheritedEnv(t *testing.T) {
	cases := []struct {
		name    string
		aiAgent string
		baseURL string
	}{
		{"no env set", "", ""},
		{"AI_AGENT set, no base url", "1", ""},
		{"AI_AGENT set, minimax base url", "1", "https://api.minimax.io/anthropic"},
		{"AI_AGENT unset, minimax base url present anyway", "", "https://api.minimax.io/anthropic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AI_AGENT", c.aiAgent)
			t.Setenv("ANTHROPIC_BASE_URL", c.baseURL)
			if got := detectAgentKind(); got != "codex-cli" {
				t.Fatalf("detectAgentKind() = %q, want %q", got, "codex-cli")
			}
		})
	}
}
