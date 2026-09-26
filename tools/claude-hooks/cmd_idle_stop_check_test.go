package main

// Tests for cmd_idle_stop_check.go (nervous-bus#36). Fixture transcript
// lines below mirror the STRUCTURAL shapes sampled from real
// ~/.claude/projects/**/*.jsonl transcripts (type/message/role/content
// field layout, isMeta flag, tool_use/tool_result block shapes,
// non-chat structural line types like "cost-state"/"bridge-session") —
// no real transcript content or session data is copied in.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- fixture line builders ---

func idleFixtureUser(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	})
	return string(b)
}

func idleFixtureUserMeta(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":   "user",
		"isMeta": true,
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	})
	return string(b)
}

func idleFixtureToolResult() string {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			},
		},
	})
	return string(b)
}

func idleFixtureAssistantText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
	return string(b)
}

func idleFixtureAssistantToolUse(name string, input map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "tool_use", "id": "toolu_x", "name": name, "input": input},
			},
		},
	})
	return string(b)
}

// idleFixtureAssistantTextAndTool combines a text block and a tool_use
// block in one assistant entry, mirroring how a real turn's final
// assistant message often carries both.
func idleFixtureAssistantTextAndTool(text, name string, input map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": text},
				{"type": "tool_use", "id": "toolu_x", "name": name, "input": input},
			},
		},
	})
	return string(b)
}

func idleFixtureStructural(typ string) string {
	b, _ := json.Marshal(map[string]any{"type": typ})
	return string(b)
}

// --- unit tests: idleStopExtractTurn / idleStopShouldFire ---

func TestIdleStopShouldFire_OnlyWaiterTurn_Blocks(t *testing.T) {
	lines := []string{
		idleFixtureUser("Kick off the deploy gate and let me know when it's green."),
		idleFixtureStructural("bridge-session"),
		idleFixtureAssistantToolUse("Bash", map[string]any{
			"command":           "until grep -q GATE_EXIT= gate.log; do sleep 30; done",
			"run_in_background": true,
		}),
		idleFixtureStructural("cost-state"),
		idleFixtureAssistantText("Armed a waiter on the gate log."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if !turn.BoundaryFound {
		t.Fatalf("expected boundary found")
	}
	if !idleStopShouldFire(turn) {
		t.Fatalf("expected fire=true for only-waiter turn")
	}
}

func TestIdleStopShouldFire_WaiterPlusRealWork_Allows(t *testing.T) {
	lines := []string{
		idleFixtureUser("Fix the flaky test and kick off the gate."),
		idleFixtureAssistantToolUse("Edit", map[string]any{
			"file_path": "pkg/foo/foo_test.go",
			"old_string": "want 1",
			"new_string": "want 2",
		}),
		idleFixtureAssistantToolUse("Bash", map[string]any{
			"command":           "until grep -q GATE_EXIT= gate.log; do sleep 30; done",
			"run_in_background": true,
		}),
		idleFixtureAssistantText("Fixed the test and armed the gate waiter."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if idleStopShouldFire(turn) {
		t.Fatalf("expected fire=false when real Edit work is present alongside a waiter")
	}
}

func TestIdleStopShouldFire_ExplicitBlockedOnStatement_Allows(t *testing.T) {
	lines := []string{
		idleFixtureUser("Ship the deploy once the gate is green."),
		idleFixtureAssistantToolUse("Monitor", map[string]any{"command": "until grep -q DONE log; do sleep 10; done"}),
		idleFixtureAssistantText("Blocked on the gate finishing; nothing else in this repo can proceed until it does."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if idleStopShouldFire(turn) {
		t.Fatalf("expected fire=false when the final text states an explicit blocked-on reason")
	}
}

func TestIdleStopShouldFire_StatusProbesOnlyNoBackground_Allows(t *testing.T) {
	// Only read-only status probes, nothing actually backgrounded — the
	// design explicitly requires a background signal too.
	lines := []string{
		idleFixtureUser("Check whether the PR is mergeable."),
		idleFixtureAssistantToolUse("Bash", map[string]any{"command": "gh pr checks 42", "run_in_background": false}),
		idleFixtureAssistantToolUse("Bash", map[string]any{"command": "git status", "run_in_background": false}),
		idleFixtureAssistantText("PR checks are pending."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if idleStopShouldFire(turn) {
		t.Fatalf("expected fire=false when nothing is actually backgrounded")
	}
}

func TestIdleStopShouldFire_NoToolCalls_Allows(t *testing.T) {
	lines := []string{
		idleFixtureUser("What's the status of the deploy?"),
		idleFixtureAssistantText("Still queued, nothing new to report."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if idleStopShouldFire(turn) {
		t.Fatalf("expected fire=false for a text-only turn with no tool calls")
	}
}

func TestIdleStopShouldFire_BackgroundAgentDispatchOnly_Blocks(t *testing.T) {
	lines := []string{
		idleFixtureUser("Dispatch the audit and let it run."),
		idleFixtureAssistantToolUse("Agent", map[string]any{
			"prompt":            "audit the thing",
			"run_in_background": true,
		}),
		idleFixtureAssistantText("Dispatched; waiting on its notification."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if !idleStopShouldFire(turn) {
		t.Fatalf("expected fire=true for a bare background Agent dispatch with no other work")
	}
}

func TestIdleStopShouldFire_ForegroundAgentDispatch_Allows(t *testing.T) {
	lines := []string{
		idleFixtureUser("Look into this bug."),
		idleFixtureAssistantToolUse("Agent", map[string]any{
			"prompt": "investigate the bug",
		}),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if idleStopShouldFire(turn) {
		t.Fatalf("expected fire=false for a foreground (non-backgrounded) Agent dispatch")
	}
}

func TestIdleStopExtractTurn_SkipsToolResultsAndInjections(t *testing.T) {
	lines := []string{
		idleFixtureUser("Real user request goes here."),
		idleFixtureAssistantToolUse("Bash", map[string]any{"command": "ls"}),
		idleFixtureToolResult(),
		idleFixtureUser("<system-reminder>\nsome injected reminder text\n</system-reminder>"),
		idleFixtureUserMeta("<local-command-caveat>caveat text</local-command-caveat>"),
		idleFixtureAssistantToolUse("Monitor", map[string]any{"command": "until grep -q DONE log; do sleep 10; done"}),
		idleFixtureAssistantText("Armed the waiter."),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if !turn.BoundaryFound {
		t.Fatalf("expected boundary found (tool_result/injection lines must not count as boundaries)")
	}
	// Both tool_use calls after the real boundary should be collected —
	// the intervening tool_result/system-reminder/meta-caveat lines must
	// not have been mistaken for the boundary.
	if len(turn.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls collected, got %d: %+v", len(turn.ToolCalls), turn.ToolCalls)
	}
}

func TestIdleStopExtractTurn_NoBoundaryFound_FailsOpen(t *testing.T) {
	// A tail window with nothing but tool_result/injection noise (no
	// genuine user boundary anywhere in it) must report BoundaryFound=false
	// rather than treat the whole window as "the turn".
	lines := []string{
		idleFixtureToolResult(),
		idleFixtureUser("<system-reminder>only an injection</system-reminder>"),
		idleFixtureAssistantToolUse("Bash", map[string]any{"command": "ls", "run_in_background": true}),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if turn.BoundaryFound {
		t.Fatalf("expected BoundaryFound=false with no genuine user line in the window")
	}
	if idleStopShouldFire(turn) {
		t.Fatalf("expected fire=false when no boundary was found (fail open)")
	}
}

func TestIdleStopExtractTurn_FinalTextIsLastAssistantOnly(t *testing.T) {
	// An earlier assistant message in the same turn has text; the final
	// one (chronologically last) has none. FinalText must reflect the
	// LAST message, not fall back to the earlier one.
	lines := []string{
		idleFixtureUser("Do the thing."),
		idleFixtureAssistantText("Blocked on nothing else being clear yet, investigating."),
		idleFixtureToolResult(),
		idleFixtureAssistantToolUse("Monitor", map[string]any{"command": "until grep -q DONE log; do sleep 10; done"}),
	}
	turn := idleStopExtractTurn([]byte(strings.Join(lines, "\n")))
	if turn.FinalText != "" {
		t.Fatalf("expected FinalText empty (last assistant entry had no text block), got %q", turn.FinalText)
	}
	// Because FinalText correctly does NOT inherit the earlier "blocked on"
	// text, this bare-Monitor turn should fire.
	if !idleStopShouldFire(turn) {
		t.Fatalf("expected fire=true; the blocked-on text belonged to an earlier message, not the final one")
	}
}

// --- classifier unit tests ---

func TestIdleStopIsAllowedCall(t *testing.T) {
	cases := []struct {
		name string
		tc   idleStopToolCall
		want bool
	}{
		{"monitor", idleStopToolCall{Name: "Monitor"}, true},
		{"schedule-wakeup", idleStopToolCall{Name: "ScheduleWakeup"}, true},
		{"task-output", idleStopToolCall{Name: "TaskOutput"}, true},
		{"workflow", idleStopToolCall{Name: "Workflow"}, true},
		{"bg-bash-sleep-loop", idleStopToolCall{Name: "Bash", RunInBackground: true, Command: "until grep -q X log; do sleep 5; done"}, true},
		{"bg-bash-bare-sleep", idleStopToolCall{Name: "Bash", RunInBackground: true, Command: "sleep 300"}, true},
		{"fg-bash-plain-sleep-not-status", idleStopToolCall{Name: "Bash", RunInBackground: false, Command: "sleep 300"}, false},
		{"bash-gh-pr-checks", idleStopToolCall{Name: "Bash", Command: "gh pr checks 12"}, true},
		{"bash-git-status", idleStopToolCall{Name: "Bash", Command: "git status"}, true},
		{"bash-tail-log", idleStopToolCall{Name: "Bash", Command: "tail -n 50 build.log"}, true},
		{"bash-real-edit-command", idleStopToolCall{Name: "Bash", Command: "sed -i s/foo/bar/ file.go"}, false},
		{"bg-agent", idleStopToolCall{Name: "Agent", RunInBackground: true}, true},
		{"fg-agent", idleStopToolCall{Name: "Agent", RunInBackground: false}, false},
		{"edit-tool", idleStopToolCall{Name: "Edit"}, false},
		{"write-tool", idleStopToolCall{Name: "Write"}, false},
		{"read-tool", idleStopToolCall{Name: "Read"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := idleStopIsAllowedCall(tc.tc); got != tc.want {
				t.Errorf("idleStopIsAllowedCall(%+v) = %v, want %v", tc.tc, got, tc.want)
			}
		})
	}
}

func TestIdleStopIsBackgroundSignal(t *testing.T) {
	cases := []struct {
		name string
		tc   idleStopToolCall
		want bool
	}{
		{"monitor", idleStopToolCall{Name: "Monitor"}, true},
		{"fg-bash-status-probe", idleStopToolCall{Name: "Bash", Command: "git status"}, false},
		{"bg-bash", idleStopToolCall{Name: "Bash", RunInBackground: true, Command: "sleep 5"}, true},
		{"bg-agent", idleStopToolCall{Name: "Agent", RunInBackground: true}, true},
		{"fg-agent", idleStopToolCall{Name: "Agent"}, false},
		{"workflow", idleStopToolCall{Name: "Workflow"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := idleStopIsBackgroundSignal(tc.tc); got != tc.want {
				t.Errorf("idleStopIsBackgroundSignal(%+v) = %v, want %v", tc.tc, got, tc.want)
			}
		})
	}
}

// --- harness tests: cmdIdleStopCheck end to end over the stdin protocol ---

// idsRunWithStdinFile writes stdinJSON to os.Stdin, runs cmdIdleStopCheck,
// and returns whatever it printed to stdout.
func idsRunWithStdinFile(t *testing.T, stdinJSON string) string {
	t.Helper()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	_, _ = w.WriteString(stdinJSON)
	_ = w.Close()
	defer func() { os.Stdin = oldStdin }()

	oldStdout := os.Stdout
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = wo

	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		_, _ = io.Copy(&buf, ro)
		close(done)
	}()

	cmdIdleStopCheck()

	_ = wo.Close()
	os.Stdout = oldStdout
	<-done
	_ = ro.Close()
	return buf.String()
}

func idsWriteFixture(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func idsStdinJSON(t *testing.T, transcriptPath string, stopHookActive bool) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":       "sess-1",
		"transcript_path":  transcriptPath,
		"stop_hook_active": stopHookActive,
	})
	if err != nil {
		t.Fatalf("marshal stdin: %v", err)
	}
	return string(b)
}

func TestCmdIdleStopCheck_OnlyWaiterTurn_Blocks(t *testing.T) {
	dir := t.TempDir()
	path := idsWriteFixture(t, dir, "sess-1.jsonl", []string{
		idleFixtureUser("Kick off the deploy gate."),
		idleFixtureAssistantToolUse("Bash", map[string]any{
			"command":           "until grep -q GATE_EXIT= gate.log; do sleep 30; done",
			"run_in_background": true,
		}),
		idleFixtureAssistantText("Armed a waiter."),
	})
	out := idsRunWithStdinFile(t, idsStdinJSON(t, path, false))
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("expected a decision JSON on stdout, got %q (err %v)", out, err)
	}
	if decoded["decision"] != "block" {
		t.Fatalf("expected decision=block, got %+v", decoded)
	}
}

func TestCmdIdleStopCheck_StopHookActive_Allows(t *testing.T) {
	dir := t.TempDir()
	path := idsWriteFixture(t, dir, "sess-1.jsonl", []string{
		idleFixtureUser("Kick off the deploy gate."),
		idleFixtureAssistantToolUse("Bash", map[string]any{
			"command":           "until grep -q GATE_EXIT= gate.log; do sleep 30; done",
			"run_in_background": true,
		}),
	})
	out := idsRunWithStdinFile(t, idsStdinJSON(t, path, true))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no output when stop_hook_active=true, got %q", out)
	}
}

func TestCmdIdleStopCheck_SubagentTranscript_Allows(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := idsWriteFixture(t, subDir, "agent-abc123.jsonl", []string{
		idleFixtureUser("Kick off the deploy gate."),
		idleFixtureAssistantToolUse("Bash", map[string]any{
			"command":           "until grep -q GATE_EXIT= gate.log; do sleep 30; done",
			"run_in_background": true,
		}),
	})
	out := idsRunWithStdinFile(t, idsStdinJSON(t, path, false))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no output for a subagent transcript path, got %q", out)
	}
}

func TestCmdIdleStopCheck_MalformedTranscript_Allows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	if err := os.WriteFile(path, []byte("{not valid json\n{{{\xff\xfe garbage"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out := idsRunWithStdinFile(t, idsStdinJSON(t, path, false))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no output for a malformed transcript, got %q", out)
	}
}

func TestCmdIdleStopCheck_MissingTranscriptPath_Allows(t *testing.T) {
	out := idsRunWithStdinFile(t, idsStdinJSON(t, "", false))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no output with empty transcript_path, got %q", out)
	}
}

func TestCmdIdleStopCheck_KillSwitch_Allows(t *testing.T) {
	t.Setenv("IDLE_STOP_CHECK_DISABLE", "1")
	dir := t.TempDir()
	path := idsWriteFixture(t, dir, "sess-1.jsonl", []string{
		idleFixtureUser("Kick off the deploy gate."),
		idleFixtureAssistantToolUse("Bash", map[string]any{
			"command":           "until grep -q GATE_EXIT= gate.log; do sleep 30; done",
			"run_in_background": true,
		}),
	})
	out := idsRunWithStdinFile(t, idsStdinJSON(t, path, false))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no output with IDLE_STOP_CHECK_DISABLE=1, got %q", out)
	}
}

func TestCmdIdleStopCheck_ExplicitBlockedOnStatement_Allows(t *testing.T) {
	dir := t.TempDir()
	path := idsWriteFixture(t, dir, "sess-1.jsonl", []string{
		idleFixtureUser("Ship it once the gate is green."),
		idleFixtureAssistantToolUse("Monitor", map[string]any{"command": "until grep -q DONE log; do sleep 10; done"}),
		idleFixtureAssistantText("Blocked on the gate finishing; nothing else can run until it does."),
	})
	out := idsRunWithStdinFile(t, idsStdinJSON(t, path, false))
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no output when the final text states blocked-on, got %q", out)
	}
}
