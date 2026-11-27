package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOrcaIdentityAndSessionBinding_Lifecycle(t *testing.T) {
	repo, _, _ := repoWorktree(t)
	calls := stubPublish(t)
	bindDir := t.TempDir()
	t.Setenv("HEARTH_BINDINGS_DIR", bindDir)
	t.Setenv("ORCA_TERMINAL_HANDLE", "term_11111111-2222-3333-4444-555555555555")
	t.Setenv("ORCA_TAB_ID", "tab-99")
	t.Setenv("ORCA_WORKTREE_ID", "wt-42")

	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-orca-1","modelName":"gemini-4-neon-medium","transcriptPath":"/tmp/t.jsonl"}`

	runSessionStart(strings.NewReader(payload))
	if len(*calls) != 2 {
		t.Fatalf("session-start publishes = %d, want 2", len(*calls))
	}
	sessEnv := decodeEnvelope(t, (*calls)[1].Payload)
	if sessEnv["pane_id"] != "orca:term_11111111-2222-3333-4444-555555555555" {
		t.Fatalf("pane_id = %v", sessEnv["pane_id"])
	}
	if sessEnv["pane_id_qualified"] != "orca:term_11111111-2222-3333-4444-555555555555" {
		t.Fatalf("pane_id_qualified = %v", sessEnv["pane_id_qualified"])
	}
	if sessEnv["orca_terminal"] != "term_11111111-2222-3333-4444-555555555555" {
		t.Fatalf("orca_terminal = %v", sessEnv["orca_terminal"])
	}
	if sessEnv["orca_tab_id"] != "tab-99" || sessEnv["orca_worktree_id"] != "wt-42" {
		t.Fatalf("orca tab/wt = %v / %v", sessEnv["orca_tab_id"], sessEnv["orca_worktree_id"])
	}
	if sessEnv["managed_by"] != "orca" {
		t.Fatalf("managed_by = %v", sessEnv["managed_by"])
	}

	readBinding := func() sessionBinding {
		raw, err := os.ReadFile(filepath.Join(bindDir, "conv-orca-1.json"))
		if err != nil {
			t.Fatalf("read binding: %v", err)
		}
		var b sessionBinding
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal binding: %v", err)
		}
		return b
	}

	b := readBinding()
	if b.Turn != "idle" || b.OrcaTerminal != "term_11111111-2222-3333-4444-555555555555" || b.Transcript != "/tmp/t.jsonl" {
		t.Fatalf("unexpected session-start binding: %+v", b)
	}

	// PreInvocation flips turn to busy (even when invocationNum > 1).
	runPreInvocation(strings.NewReader(`{"workspacePaths":["` + repo + `"],"conversationId":"conv-orca-1","invocationNum":2}`))
	b = readBinding()
	if b.Turn != "busy" || b.Transcript != "/tmp/t.jsonl" {
		t.Fatalf("unexpected pre-invocation binding: %+v", b)
	}

	// Stop flips turn back to idle.
	runStop(strings.NewReader(payload))
	b = readBinding()
	if b.Turn != "idle" {
		t.Fatalf("unexpected stop binding: %+v", b)
	}
}

func TestStopHarvestsNeedsYou_PublishesAttentionAndWritesState(t *testing.T) {
	repo, project, _ := repoWorktree(t)
	calls := stubPublish(t)
	nyHome := t.TempDir()
	t.Setenv("NEEDS_YOU_HOME", nyHome)
	t.Setenv("ORCA_TERMINAL_HANDLE", "term_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	// Write a custom session name.
	namesDir := filepath.Join(nyHome, "names")
	if err := os.MkdirAll(namesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namesDir, "conv-ny-1"), []byte("agy-parity-lane\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write an agy transcript.jsonl with USER_INPUT, a tool-calling PLANNER_RESPONSE, and a final reply.
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	transcriptLines := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","content":"Check deploy"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","content":"Running check","tool_calls":[{"id":"c1","name":"run_command"}]}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","content":"Staging build is green and ready to promote.\n\n**Needs you:**\n- Approve production rollout for dec-42\n- Confirm DNS cutover hearth://domains/check"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcriptLines), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-ny-1","modelName":"gemini-4-neon-medium","transcriptPath":"` + transcriptPath + `"}`
	runStop(strings.NewReader(payload))

	// Expect 3 publishes: bus.agent.activity.v1, agent.session.v1, agent.attention.v1.
	if len(*calls) != 3 {
		t.Fatalf("expected 3 publishes, got %d: %+v", len(*calls), *calls)
	}
	attCall := (*calls)[2]
	if attCall.Channel != chanAgentAttention {
		t.Fatalf("channel = %q, want %q", attCall.Channel, chanAgentAttention)
	}
	if attCall.Source != needsYouSource {
		t.Fatalf("source = %q, want %q", attCall.Source, needsYouSource)
	}

	var att attentionPayload
	if err := json.Unmarshal([]byte(attCall.Payload), &att); err != nil {
		t.Fatalf("unmarshal attention: %v", err)
	}
	wantItems := []string{
		"Approve production rollout for dec-42 <hearth://decisions/dec-42>",
		"Confirm DNS cutover <hearth://domains/check>",
	}
	if att.SessionID != "conv-ny-1" || att.Project != project || att.CWD != repo {
		t.Fatalf("identity mismatch: %+v", att)
	}
	if att.Name != "agy-parity-lane" || att.OrcaTerminal != "term_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("name/orca mismatch: %+v", att)
	}
	if att.Summary != "Staging build is green and ready to promote." {
		t.Fatalf("summary = %q", att.Summary)
	}
	if !reflect.DeepEqual(att.Items, wantItems) {
		t.Fatalf("items = %#v, want %#v", att.Items, wantItems)
	}

	// Verify state file written for `needs-you list` and Kiln's needs-you-feed.
	stateRaw, err := os.ReadFile(filepath.Join(nyHome, "sessions", "conv-ny-1.json"))
	if err != nil {
		t.Fatalf("read needs-you state: %v", err)
	}
	var entry needsYouStateEntry
	if err := json.Unmarshal(stateRaw, &entry); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if entry.Acked || !reflect.DeepEqual(entry.Items, wantItems) || entry.Name != "agy-parity-lane" {
		t.Fatalf("unexpected state entry: %+v", entry)
	}
}

func TestStopHarvestsNeedsYouNothing_ClearsWithEmptyItems(t *testing.T) {
	repo, _, _ := repoWorktree(t)
	calls := stubPublish(t)
	nyHome := t.TempDir()
	t.Setenv("NEEDS_YOU_HOME", nyHome)

	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := `{"type":"USER_INPUT","content":"hi"}` + "\n" +
		`{"type":"PLANNER_RESPONSE","content":"All tests pass.\n\n**Needs you:** nothing"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	runStop(strings.NewReader(`{"workspacePaths":["` + repo + `"],"conversationId":"conv-clear","transcriptPath":"` + transcriptPath + `"}`))
	if len(*calls) != 3 {
		t.Fatalf("expected 3 publishes, got %d", len(*calls))
	}
	// Verify JSON literally carries `"items":[]` (not null/omitted).
	if !strings.Contains((*calls)[2].Payload, `"items":[]`) {
		t.Fatalf("expected empty items array in %s", (*calls)[2].Payload)
	}
}

func TestStopNeedsYouDisabled_SkipsAttention(t *testing.T) {
	repo, _, _ := repoWorktree(t)
	calls := stubPublish(t)
	t.Setenv("NEEDS_YOU_DISABLE", "1")

	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := `{"type":"PLANNER_RESPONSE","content":"Done.\n\n**Needs you:**\n- Check this"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	runStop(strings.NewReader(`{"workspacePaths":["` + repo + `"],"conversationId":"conv-dis","transcriptPath":"` + transcriptPath + `"}`))
	if len(*calls) != 2 {
		t.Fatalf("expected 2 publishes when NEEDS_YOU_DISABLE=1, got %d", len(*calls))
	}
}

func TestDefaultSessionName_WorktreeAndOrcaPaths(t *testing.T) {
	if got := defaultSessionName("/home/eric/data2/orca/claude-hook-fast/neon-agy-parity"); got != "neon-agy-parity" {
		t.Fatalf("orca path name = %q", got)
	}
	if got := defaultSessionName("/home/eric/data2/worktrees/hearth/agent-abcdef123456"); got != "" {
		t.Fatalf("agent-hex path should return empty, got %q", got)
	}
	if got := defaultSessionName("/home/eric/projects/hearth"); got != "" {
		t.Fatalf("direct checkout should return empty, got %q", got)
	}
}

func TestPreAndPostTool_AskQuestionEmitsQuestionAndResponded(t *testing.T) {
	repo, project, _ := repoWorktree(t)
	calls := stubPublish(t)
	t.Setenv("ORCA_TERMINAL_HANDLE", "term_12345678-1234-1234-1234-1234567890ab")

	payload := `{
		"workspacePaths":["` + repo + `"],
		"conversationId":"conv-q1",
		"toolCall":{
			"id":"call_q_42",
			"name":"ask_question",
			"args":{
				"questions":[
					{"question":"Which mode?","options":["Fast (Recommended)","Safe"],"is_multi_select":false}
				]
			}
		}
	}`

	runPreTool(strings.NewReader(payload))
	if len(*calls) != 1 || (*calls)[0].Channel != chanSessionQuestion {
		t.Fatalf("pre-tool ask_question calls = %+v", *calls)
	}
	qEnv := decodeEnvelope(t, (*calls)[0].Payload)
	if qEnv["correlation_id"] != "call_q_42" || qEnv["tool_use_id"] != "call_q_42" {
		t.Fatalf("correlation mismatch: %+v", qEnv)
	}
	if qEnv["question"] != "Which mode?" || qEnv["project"] != project {
		t.Fatalf("question/project mismatch: %+v", qEnv)
	}
	if qEnv["pane_id"] != "orca:term_12345678-1234-1234-1234-1234567890ab" || qEnv["managed_by"] != "orca" {
		t.Fatalf("orca identity missing: %+v", qEnv)
	}

	runPostTool(strings.NewReader(payload))
	if len(*calls) != 3 {
		t.Fatalf("expected activity + question.responded, got %d calls: %+v", len(*calls), *calls)
	}
	if (*calls)[2].Channel != chanSessionQuestionResponded {
		t.Fatalf("last channel = %q, want %q", (*calls)[2].Channel, chanSessionQuestionResponded)
	}
	rEnv := decodeEnvelope(t, (*calls)[2].Payload)
	if rEnv["correlation_id"] != "call_q_42" || rEnv["answered_by"] != "terminal" {
		t.Fatalf("responded envelope = %+v", rEnv)
	}
}

func TestPreAndPostTool_InvokeSubagentEmitsLifecycle(t *testing.T) {
	repo, _, _ := repoWorktree(t)
	calls := stubPublish(t)

	payload := `{
		"workspacePaths":["` + repo + `"],
		"conversationId":"conv-sub1",
		"toolCall":{
			"id":"call_sub_7",
			"name":"invoke_subagent",
			"args":{"TypeName":"research","Role":"Codebase Researcher","Prompt":"inspect"}
		}
	}`

	runPreTool(strings.NewReader(payload))
	if len(*calls) != 1 || (*calls)[0].Channel != chanSubagentLifecycle {
		t.Fatalf("pre-tool subagent calls = %+v", *calls)
	}
	startEnv := decodeEnvelope(t, (*calls)[0].Payload)
	if startEnv["event"] != "subagent_started" || startEnv["subagent_type"] != "research" || startEnv["subagent_id"] != "call_sub_7" {
		t.Fatalf("subagent_started = %+v", startEnv)
	}

	runPostTool(strings.NewReader(payload))
	if len(*calls) != 3 || (*calls)[2].Channel != chanSubagentLifecycle {
		t.Fatalf("post-tool subagent calls = %+v", *calls)
	}
	stopEnv := decodeEnvelope(t, (*calls)[2].Payload)
	if stopEnv["event"] != "subagent_stopped" || stopEnv["subagent_id"] != "call_sub_7" {
		t.Fatalf("subagent_stopped = %+v", stopEnv)
	}
}

func TestPostTool_ToolUseIDAndFileResources(t *testing.T) {
	repo, _, _ := repoWorktree(t)
	calls := stubPublish(t)

	payload := `{
		"workspacePaths":["` + repo + `"],
		"conversationId":"conv-res1",
		"toolCall":{"id":"call_rf_9","name":"read_file","args":{"Path":"cmd/agy-hook/main.go"}}
	}`
	runPostTool(strings.NewReader(payload))
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	got := decodeEnvelope(t, (*calls)[0].Payload)
	if got["tool_use_id"] != "call_rf_9" {
		t.Fatalf("tool_use_id = %v", got["tool_use_id"])
	}
	wantRes := []any{map[string]any{"intent": "read", "ref": "file:" + filepath.Join(repo, "cmd/agy-hook/main.go")}}
	if !reflect.DeepEqual(got["resources"], wantRes) {
		t.Fatalf("resources = %#v, want %#v", got["resources"], wantRes)
	}
}

func TestCLIStdoutContracts_PreToolAndSessionStart(t *testing.T) {
	stubPublish(t)
	var out bytes.Buffer
	cmd := newAgyHookCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"pre-tool"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("pre-tool execute: %v", err)
	}
	if strings.TrimSpace(out.String()) != `{"decision":"allow"}` {
		t.Fatalf("pre-tool stdout = %q", out.String())
	}

	out.Reset()
	cmd = newAgyHookCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"session-start"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("session-start execute: %v", err)
	}
	if strings.TrimSpace(out.String()) != `{}` {
		t.Fatalf("session-start stdout = %q", out.String())
	}
}
