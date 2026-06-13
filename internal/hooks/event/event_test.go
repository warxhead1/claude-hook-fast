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
