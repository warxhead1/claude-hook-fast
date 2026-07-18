package event

import (
	"testing"
)

func TestParse_ClaudeCodeShape(t *testing.T) {
	raw := []byte(`{"session_id":"sess-abc","cwd":"/x","tool_name":"Bash","transcript_path":"/p/conv-xyz.jsonl","parent_tool_use_id":"par-1"}`)
	h := Parse(raw)
	if h.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q", h.SessionID)
	}
	if h.ToolName != "Bash" {
		t.Errorf("ToolName = %q", h.ToolName)
	}
	if h.AgentKind() != "host_subagent" {
		t.Errorf("AgentKind = %q", h.AgentKind())
	}
	if got := h.ConversationID(); got != "conv-xyz" {
		t.Errorf("ConversationID = %q", got)
	}
}

func TestParse_OpencodeAliases(t *testing.T) {
	raw := []byte(`{"sessionID":"sess-oc","tool":"edit"}`)
	h := Parse(raw)
	if h.SessionID != "sess-oc" {
		t.Errorf("SessionID alias not picked up: %q", h.SessionID)
	}
	if h.ToolName != "edit" {
		t.Errorf("ToolName alias not picked up: %q", h.ToolName)
	}
	if h.AgentKind() != "host_claude_code" {
		t.Errorf("AgentKind = %q, want host_claude_code (no parent)", h.AgentKind())
	}
}

func TestAgentKind_SubagentByTranscriptPath(t *testing.T) {
	// parent_tool_use_id absent (Claude Code omits it on Stop/SubagentStop),
	// but the transcript path self-identifies a subagent → host_subagent.
	cases := []struct {
		name       string
		transcript string
		want       string
	}{
		{"plain subagent", "/home/eric/.claude/projects/-home-eric-x/abc/subagents/agent-a65396b02f1705f90.jsonl", "host_subagent"},
		{"workflow subagent", "/home/eric/.claude/projects/-home-eric-x/abc/subagents/workflows/wf_1/agent-x.jsonl", "host_subagent"},
		{"top-level session", "/home/eric/.claude/projects/-home-eric-x/55483a8f-1234.jsonl", "host_claude_code"},
		{"no transcript", "", "host_claude_code"},
	}
	for _, c := range cases {
		h := HookInput{TranscriptPath: c.transcript}
		if got := h.AgentKind(); got != c.want {
			t.Errorf("%s: AgentKind = %q, want %q", c.name, got, c.want)
		}
		if got := h.IsSubagent(); got != (c.want == "host_subagent") {
			t.Errorf("%s: IsSubagent = %v", c.name, got)
		}
	}
}

func TestParse_EmptyDoesNotPanic(t *testing.T) {
	h := Parse(nil)
	if h.SessionID != "" {
		t.Errorf("expected zero value, got %+v", h)
	}
	h = Parse([]byte("not json"))
	if h.SessionID != "" {
		t.Errorf("expected zero value on bad JSON, got %+v", h)
	}
}

func TestEffectiveCWD_FallsBack(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "/from/env")
	h := HookInput{}
	if got := h.EffectiveCWD(); got != "/from/env" {
		t.Errorf("EffectiveCWD = %q, want /from/env", got)
	}
	h = HookInput{CWD: "/from/payload"}
	if got := h.EffectiveCWD(); got != "/from/payload" {
		t.Errorf("EffectiveCWD = %q, want /from/payload", got)
	}
}
