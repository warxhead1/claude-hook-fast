package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPayloadDecodePreservesCodex0149Fields(t *testing.T) {
	p, err := readPayloadFrom(strings.NewReader(`{"session_id":"root-uuid","agent_id":"child-1","agent_type":"subagent","transcript_path":"/tmp/root.jsonl","agent_transcript_path":"/tmp/child.jsonl","cwd":"/repo","hook_event_name":"PostToolUse","model":"gpt-5.6","permission_mode":"on-request","turn_id":"turn-7","source":"resume","reason":"done","tool_name":"Bash","tool_input":{"command":"git status --token secret"},"tool_response":{"stdout":"private"},"tool_use_id":"tool-1","stop_hook_active":true,"last_assistant_message":"finished","prompt":"please inspect","trigger":"manual"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.SessionID != "root-uuid" || p.AgentID != "child-1" || p.AgentTranscriptPath != "/tmp/child.jsonl" || p.ToolUseID != "tool-1" || !p.StopHookActive {
		t.Fatalf("field loss: %+v", p)
	}
	if got := codexToolSummary(p); strings.Contains(got, "secret") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("unsafe summary %q", got)
	}
}

func TestPayloadUsesCurrentToolInputAndRejectsOversize(t *testing.T) {
	p, err := readPayloadFrom(strings.NewReader(`{"tool_input":{"command":"new"},"input":{"command":"old"}}`))
	if err != nil || string(p.effectiveInput()) != `{"command":"new"}` {
		t.Fatalf("payload=%+v err=%v", p, err)
	}
	tooLarge := `{"tool_input":"` + strings.Repeat("x", maxHookPayloadBytes) + `"}`
	if p, err := readPayloadFrom(strings.NewReader(tooLarge)); err == nil || p != nil {
		t.Fatalf("oversize accepted payload=%v err=%v", p, err)
	}
}

func TestStopIsTurnEndAndSessionEndIsTerminal(t *testing.T) {
	p := &hookPayload{SessionID: "root", TurnID: "t1"}
	stop, _ := buildEnvelope(p, "turn_end")
	ended, _ := buildEnvelope(p, "ended")
	if stop["event"] != "turn_end" || ended["event"] != "ended" {
		t.Fatalf("bad lifecycle stop=%v end=%v", stop["event"], ended["event"])
	}
	if stop["event_id"] == ended["event_id"] {
		t.Fatal("distinct semantic lifecycle ids required")
	}
}

func TestWireEventsFollowNervousSchemas(t *testing.T) {
	for _, tc := range []struct {
		action    string
		lifecycle bool
		want      string
	}{
		{"started", true, "started"}, {"turn_end", true, "turn_end"}, {"compact_pre", true, "compact"},
		{"lease_refresh", true, "updated"}, {"ended", true, "ended"}, {"tool_pre", false, "tool_call"},
		{"tool_post", false, "tool_return"}, {"user_prompt", false, "heartbeat"}, {"permission_requested", false, "permission_requested"},
	} {
		if got := wireEvent(tc.action, tc.lifecycle); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.action, got, tc.want)
		}
	}
}

func TestEmitSeparatesWireEventFromHookAction(t *testing.T) {
	old := publishAsync
	defer func() { publishAsync = old }()
	var got map[string]any
	publishAsync = func(_ string, _ string, payload string) error { return json.Unmarshal([]byte(payload), &got) }
	emitHook(&hookPayload{SessionID: "root", TurnID: "t"}, "lease_refresh", true)
	if got["event"] != "updated" || got["hook_action"] != "lease_refresh" {
		t.Fatalf("bad envelope %v", got)
	}
}

func TestLeaseEventIDChangesPerBucketOnly(t *testing.T) {
	p := &hookPayload{SessionID: "root", TurnID: "t"}
	base := time.Unix(300, 0)
	a := eventID(p, "updated", "lease_refresh", base)
	same := eventID(p, "updated", "lease_refresh", base.Add(29*time.Second))
	next := eventID(p, "updated", "lease_refresh", base.Add(30*time.Second))
	if a != same {
		t.Fatalf("same bucket ids differ: %s %s", a, same)
	}
	if a == next {
		t.Fatal("next lease bucket was deduped")
	}
}

func TestRootOwnsQualifiedPaneSubagentDoesNot(t *testing.T) {
	t.Setenv("TMUX_PANE", "%17")
	root, _ := buildEnvelope(&hookPayload{SessionID: "root", AgentID: "root"}, "started")
	if root["pane_binding"] != "tmux:%17" || root["pane_id"] != "%17" {
		t.Fatalf("root binding=%v", root)
	}
	child, _ := buildEnvelope(&hookPayload{SessionID: "root", AgentID: "child", AgentType: "subagent"}, "subagent_started")
	if child["root_session_id"] != "root" || child["agent_id"] != "child" || child["pane_id"] != nil {
		t.Fatalf("child stole pane: %v", child)
	}
}

func TestSubagentTranscriptFailsPaneClaimClosed(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	t.Setenv("TMUX_PANE", "%17")
	p := &hookPayload{SessionID: "inherited-root", TranscriptPath: `C:\work\subagents\agent.jsonl`}
	if !isSubagent(p) {
		t.Fatal("subagent transcript with inherited session was not detected")
	}
	if allowPaneEmission(p, "started") {
		t.Fatal("transcript-only child claimed pane")
	}
}

func TestDuplicateAndReorderedCallsHaveStableLogicalIDs(t *testing.T) {
	p := &hookPayload{SessionID: "root", TurnID: "7", ToolUseID: "tool"}
	if stableEventID(p, "tool_pre") != stableEventID(p, "tool_pre") {
		t.Fatal("duplicate event id changed")
	}
	if stableEventID(p, "tool_pre") == stableEventID(p, "tool_post") {
		t.Fatal("different semantic events collided")
	}
	if permissionCorrelationID(p) != permissionCorrelationID(p) {
		t.Fatal("duplicate permission correlation changed")
	}
}

func TestHookHotPathIsBounded(t *testing.T) {
	old := publishAsync
	defer func() { publishAsync = old }()
	publishAsync = func(string, string, string) error { return nil }
	p := &hookPayload{SessionID: "root", TurnID: "1", ToolInput: json.RawMessage(`{"command":"git status"}`)}
	start := time.Now()
	for i := 0; i < 100; i++ {
		emitHook(p, "tool_pre", false)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("100 hook emissions took %s", elapsed)
	}
}
