package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

const (
	sjSession  = "11111111-2222-3333-4444-555555555555"
	sjDispatch = "toolu_01DispatchAAAA"
	sjHex      = "a157801b8bc07de7e"
)

type sjEnv struct {
	home       string
	transcript string
	cwd        string
}

// sjSetup builds a hermetic HOME, a parent transcript path and (optionally) the
// subagent's meta.json beside it.
func sjSetup(t *testing.T, withMeta bool) sjEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := filepath.Join(home, ".claude", "projects", "-p")
	tr := filepath.Join(proj, sjSession+".jsonl")
	if err := os.MkdirAll(filepath.Join(proj, sjSession, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if withMeta {
		meta := fmt.Sprintf(`{"agentType":"general-purpose","toolUseId":%q,"spawnDepth":1}`, sjDispatch)
		if err := os.WriteFile(filepath.Join(proj, sjSession, "subagents", "agent-"+sjHex+".meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sjEnv{home: home, transcript: tr, cwd: "/x/proj/.claude/worktrees/agent-" + sjHex}
}

// subHook is a hook payload as Claude Code sends it inside a subagent: the
// PARENT's session and transcript, the subagent's agent_id, no parent_tool_use_id.
func (e sjEnv) subHook(tool, toolUseID string) event.HookInput {
	raw := map[string]any{
		"session_id": sjSession, "transcript_path": e.transcript, "cwd": e.cwd,
		"agent_id": sjHex, "agent_type": "general-purpose", "tool_name": tool,
		"tool_use_id": toolUseID, "tool_input": map[string]any{"command": "ls"},
	}
	b, _ := json.Marshal(raw)
	return event.Parse(b)
}

func (e sjEnv) parentHook(tool, toolUseID string, resp any, isErr bool) event.HookInput {
	raw := map[string]any{
		"session_id": sjSession, "transcript_path": e.transcript, "cwd": "/home/eric/projects/x",
		"tool_name": tool, "tool_use_id": toolUseID, "tool_response": resp, "tool_is_error": isErr,
		"tool_input": map[string]any{"description": "d", "prompt": "p", "subagent_type": "general-purpose"},
	}
	b, _ := json.Marshal(raw)
	return event.Parse(b)
}

func notif(toolUseID, hex, status string) string {
	return fmt.Sprintf("<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<status>%s</status>\n<summary>Agent \"x\" finished</summary>\n</task-notification>", hex, toolUseID, status)
}

func ended(ps []published) []published {
	var out []published
	for _, p := range ps {
		if p.channel == chanAgentActivity && p.payload["event"] == "ended" {
			out = append(out, p)
		}
	}
	return out
}

func TestJoin_EveryActivityEventCarriesAssignmentID(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	in := e.subHook("Bash", "toolu_B1")
	env, _ := buildActivityEnvelope(in)
	publishNbusEnvelope(chanAgentActivity, env, "p")
	lenv, _ := baseEnvelope(in, "subagent_stopped", in.AgentKind())
	publishNbusEnvelope(chanSubagentLifecycle, lenv, "p")
	got := get()
	if len(got) != 2 {
		t.Fatalf("published %d, want 2", len(got))
	}
	for _, p := range got {
		if p.payload["assignment_id"] != sjDispatch || p.payload["parent_tool_use_id"] != sjDispatch {
			t.Errorf("%s event %v: assignment_id/parent_tool_use_id = %v/%v, want %s",
				p.channel, p.payload["event"], p.payload["assignment_id"], p.payload["parent_tool_use_id"], sjDispatch)
		}
		if p.payload["subagent_id"] != sjHex {
			t.Errorf("subagent_id = %v", p.payload["subagent_id"])
		}
	}
}

func TestJoin_KeyEqualsAssignmentIDMintedAtDispatch(t *testing.T) {
	e := sjSetup(t, true)
	a := buildAssignment("Agent", sjDispatch, sjSession, json.RawMessage(`{"description":"d","prompt":"p"}`))
	env, _ := buildActivityEnvelope(e.subHook("Bash", "toolu_B1"))
	if a["assignment_id"] != env["assignment_id"] {
		t.Fatalf("activity assignment_id %v != dispatch assignment_id %v", env["assignment_id"], a["assignment_id"])
	}
}

func TestJoin_PayloadParentToolUseIDWinsOverMeta(t *testing.T) {
	e := sjSetup(t, true)
	in := e.subHook("Bash", "toolu_B1")
	in.ParentToolUseID = "toolu_FromPayload"
	if got := resolveDispatchID(in); got != "toolu_FromPayload" {
		t.Fatalf("got %q", got)
	}
}

func TestJoin_MissingMetaFallsBackToRegistryThenNothing(t *testing.T) {
	e := sjSetup(t, false)
	in := e.subHook("Bash", "toolu_B1")
	if got := resolveDispatchID(in); got != "" {
		t.Fatalf("no meta, no registry: got %q, want empty", got)
	}
	env, _ := buildActivityEnvelope(in)
	if _, ok := env["assignment_id"]; ok {
		t.Fatal("assignment_id emitted without a source")
	}
	if env["subagent_id"] != sjHex {
		t.Fatalf("subagent_id = %v, want hex even when unjoined", env["subagent_id"])
	}
	// The parent's Agent PostToolUse (background launch) writes the mapping.
	handleAgentToolReturn(e.parentHook("Agent", sjDispatch, map[string]any{"status": "async_launched", "isAsync": true, "agentId": sjHex}, false))
	if got := resolveDispatchID(in); got != sjDispatch {
		t.Fatalf("registry fallback: got %q, want %s", got, sjDispatch)
	}
}

func TestJoin_HexFromCwdWhenPayloadHasNoAgentID(t *testing.T) {
	e := sjSetup(t, true)
	in := e.subHook("Bash", "toolu_B1")
	in.AgentID = ""
	if got := resolveDispatchID(in); got != sjDispatch {
		t.Fatalf("cwd-derived hex: got %q", got)
	}
}

func TestJoin_HumanNamedLaneWithoutAgentIDIsUnjoined(t *testing.T) {
	e := sjSetup(t, true)
	in := e.subHook("Bash", "toolu_B1")
	in.AgentID = ""
	in.CWD = "/x/proj/.claude/worktrees/board-tab"
	if got := resolveDispatchID(in); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestEnded_OneRunYieldsExactlyOneEndedEvent(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	handleAgentToolReturn(e.parentHook("Agent", sjDispatch, map[string]any{"status": "async_launched", "isAsync": true, "agentId": sjHex}, false))
	for i := 0; i < 5; i++ { // tool calls
		env, _ := buildActivityEnvelope(e.subHook("Bash", fmt.Sprintf("toolu_B%d", i)))
		publishNbusEnvelope(chanAgentActivity, env, "p")
	}
	for i := 0; i < 40; i++ { // repeated mid-run SubagentStop
		lenv, _ := baseEnvelope(e.subHook("", ""), "subagent_stopped", "host_subagent")
		publishNbusEnvelope(chanSubagentLifecycle, lenv, "p")
	}
	if n := len(ended(get())); n != 0 {
		t.Fatalf("ended before completion: %d", n)
	}
	n := e.parentHook("", "", nil, false)
	n.Prompt = notif(sjDispatch, sjHex, "completed")
	for i := 0; i < 5; i++ { // the harness re-notifies on every resume-and-stop
		handleTaskNotifications(n, n.Prompt)
	}
	got := ended(get())
	if len(got) != 1 {
		t.Fatalf("ended events = %d, want 1", len(got))
	}
	p := got[0].payload
	if p["status"] != "completed" || p["parent_tool_use_id"] != sjDispatch || p["assignment_id"] != sjDispatch || p["agent_kind"] != "host_subagent" {
		t.Fatalf("bad ended payload: %v", p)
	}
	for _, k := range []string{"worktree", "display", "cwd"} {
		if _, ok := p[k]; ok {
			t.Errorf("ended carries parent's %s", k)
		}
	}
}

func TestEnded_ForegroundReturnThenNotificationIsStillOne(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	handleAgentToolReturn(e.parentHook("Agent", sjDispatch, map[string]any{"status": "completed", "agentId": sjHex}, false))
	handleAgentToolReturn(e.parentHook("Agent", sjDispatch, map[string]any{"status": "completed", "agentId": sjHex}, false))
	handleTaskNotifications(e.parentHook("", "", nil, false), notif(sjDispatch, sjHex, "completed"))
	if got := ended(get()); len(got) != 1 {
		t.Fatalf("ended events = %d, want 1", len(got))
	}
}

func TestEnded_StatusMapping(t *testing.T) {
	cases := []struct{ in, want string }{{"completed", "completed"}, {"failed", "failed"}, {"killed", "cancelled"}, {"", "completed"}}
	for _, c := range cases {
		if got := endedStatus(c.in); got != c.want {
			t.Errorf("endedStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	e := sjSetup(t, true)
	get := capturePublish(t)
	handleAgentToolReturn(e.parentHook("Agent", sjDispatch, map[string]any{"status": "completed", "agentId": sjHex}, true))
	got := ended(get())
	if len(got) != 1 || got[0].payload["status"] != "failed" {
		t.Fatalf("tool_is_error must end as failed: %v", got)
	}
}

func TestEnded_TerminalFirstStatusStandsAgainstStragglers(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	n := e.parentHook("", "", nil, false)
	handleTaskNotifications(n, notif(sjDispatch, sjHex, "completed"))
	env, _ := buildActivityEnvelope(e.subHook("Bash", "toolu_late"))
	publishNbusEnvelope(chanAgentActivity, env, "p")
	handleTaskNotifications(n, notif(sjDispatch, sjHex, "failed"))
	got := ended(get())
	if len(got) != 1 || got[0].payload["status"] != "completed" {
		t.Fatalf("first terminal status must stand, exactly once: %v", got)
	}
}

func TestEnded_NonSubagentNotificationIsIgnored(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	bg := "<task-notification>\n<task-id>bx1</task-id>\n<tool-use-id>toolu_bash1</tool-use-id>\n<status>completed</status>\n<summary>Background command \"sleep\" completed</summary>\n</task-notification>"
	handleTaskNotifications(e.parentHook("", "", nil, false), bg)
	if got := ended(get()); len(got) != 0 {
		t.Fatalf("background Bash notification produced %d ended events", len(got))
	}
}

func TestEnded_MalformedNotificationsAreSafe(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	in := e.parentHook("", "", nil, false)
	for _, s := range []string{"", "<task-notification>", "<task-notification></task-notification>",
		"<task-notification><tool-use-id>../../etc/x</tool-use-id><status>completed</status><summary>Agent \"x\"</summary></task-notification>",
		"<task-notification><tool-use-id>toolu_1</tool-use-id></task-notification>"} {
		handleTaskNotifications(in, s)
	}
	if got := ended(get()); len(got) != 0 {
		t.Fatalf("malformed input produced ended: %v", got)
	}
}

func TestEnded_DispatchMarkerAdmitsNotificationWithoutAgentSummary(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	recordDispatch(sjDispatch)
	blk := "<task-notification><task-id>" + sjHex + "</task-id><tool-use-id>" + sjDispatch + "</tool-use-id><status>completed</status><summary>renamed wording</summary></task-notification>"
	handleTaskNotifications(e.parentHook("", "", nil, false), blk)
	if got := ended(get()); len(got) != 1 {
		t.Fatalf("ended = %d, want 1", len(got))
	}
}

func TestEnded_TranscriptTailCatchesQueuedNotificationsOnce(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	q, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "prompt": notif(sjDispatch, sjHex, "completed")}})
	quoted, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": notif("toolu_Quoted", "zz", "completed")}})
	body := string(quoted) + "\n" + string(q) + "\n" + string(q) + "\n"
	if err := os.WriteFile(e.transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	in := e.parentHook("Bash", "toolu_p1", map[string]any{}, false)
	scanTranscriptNotifications(in)
	scanTranscriptNotifications(in)
	got := ended(get())
	if len(got) != 1 || got[0].payload["parent_tool_use_id"] != sjDispatch {
		t.Fatalf("ended = %v, want exactly one for %s (assistant-quoted tag must be ignored)", got, sjDispatch)
	}
}

func TestEnded_TranscriptScanSkipsSubagentTranscripts(t *testing.T) {
	e := sjSetup(t, true)
	get := capturePublish(t)
	in := e.parentHook("Bash", "toolu_p1", map[string]any{}, false)
	in.TranscriptPath = filepath.Join(filepath.Dir(e.transcript), sjSession, "subagents", "agent-"+sjHex+".jsonl")
	q, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": notif(sjDispatch, sjHex, "completed")}})
	_ = os.WriteFile(in.TranscriptPath, q, 0o644)
	scanTranscriptNotifications(in)
	if got := ended(get()); len(got) != 0 {
		t.Fatalf("subagent transcript scanned: %v", got)
	}
}

func TestEnded_ClaimIsOnceAndRejectsUnsafeIDs(t *testing.T) {
	sjSetup(t, true)
	n := 0
	for i := 0; i < 20; i++ {
		if joinRegClaim("ended", "toolu_race") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("claims = %d, want 1", n)
	}
	if joinRegPath("ended", "../x") != "" || strings.Contains(joinRegPath("ended", "a/b"), "a/b") {
		t.Fatal("unsafe id accepted")
	}
}
