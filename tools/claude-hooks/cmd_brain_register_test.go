package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ---------- brainExtractConvID ----------

func TestBrainExtractConvID_ConversationsPattern(t *testing.T) {
	path := "/home/user/.claude/projects/foo/conversations/a1b2c3d4-e5f6-7890-abcd-ef1234567890/transcript.jsonl"
	got := brainExtractConvID(path)
	want := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if got != want {
		t.Errorf("conversations pattern: got %q want %q", got, want)
	}
}

func TestBrainExtractConvID_SplitFallback(t *testing.T) {
	// Path where "conversations" is a component but not matching the regex
	// because there's no trailing slash after the id (edge case).
	// Split fallback should still find it.
	path := "/home/user/.claude/conversations/myconvid/file.jsonl"
	got := brainExtractConvID(path)
	// Regex: "conversations/myconvid/" — needs trailing slash, so won't match.
	// Split fallback: parts[i]=="conversations" → parts[i+1]=="myconvid".
	want := "myconvid"
	if got != want {
		t.Errorf("split fallback: got %q want %q", got, want)
	}
}

func TestBrainExtractConvID_Empty(t *testing.T) {
	got := brainExtractConvID("")
	if got != "" {
		t.Errorf("empty path: expected %q, got %q", "", got)
	}
}

func TestBrainExtractConvID_NoConversations(t *testing.T) {
	path := "/home/user/.claude/projects/tengine/a1b2c3d4.jsonl"
	got := brainExtractConvID(path)
	// Neither regex nor split will match — return "".
	if got != "" {
		t.Errorf("no conversations component: expected empty, got %q", got)
	}
}

// ---------- brainUpdateState ----------

func TestBrainUpdateState_WritesAndMerges(t *testing.T) {
	dir := t.TempDir()

	origDir := brainStateDir
	brainStateDir = dir
	defer func() { brainStateDir = origDir }()

	sessionId := "test-session-abc"

	// First write: base state with conversationId=null.
	var convIdNil *string
	brainUpdateState(sessionId, map[string]any{
		"agentId":          "claude-code-test",
		"conversationId":   convIdNil,
		"sessionStartTime": int64(12345),
		"cwd":              "/tmp",
		"toolsUsed":        []string{},
		"filesModified":    []string{},
	})

	path := brainStateFile(sessionId)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("state JSON invalid: %v", err)
	}

	if state["agentId"] != "claude-code-test" {
		t.Errorf("agentId mismatch: %v", state["agentId"])
	}
	if _, ok := state["conversationId"]; !ok {
		t.Error("conversationId key missing")
	}
	if state["conversationId"] != nil {
		t.Errorf("conversationId should be null when convId empty, got %v", state["conversationId"])
	}

	// Verify toolsUsed / filesModified serialize as arrays (not null).
	stateStr := string(data)
	if strings.Contains(stateStr, `"toolsUsed":null`) {
		t.Error("toolsUsed should be [] not null")
	}
	if strings.Contains(stateStr, `"filesModified":null`) {
		t.Error("filesModified should be [] not null")
	}

	// Second write (merge): add loadedContext.
	brainUpdateState(sessionId, map[string]any{
		"loadedContext": "some context",
	})

	data2, _ := os.ReadFile(path)
	var state2 map[string]any
	_ = json.Unmarshal(data2, &state2)

	if state2["agentId"] != "claude-code-test" {
		t.Error("first write fields should survive merge")
	}
	if state2["loadedContext"] != "some context" {
		t.Errorf("loadedContext not merged: %v", state2["loadedContext"])
	}
}

func TestBrainUpdateState_DefaultSessionKey(t *testing.T) {
	dir := t.TempDir()
	origDir := brainStateDir
	brainStateDir = dir
	defer func() { brainStateDir = origDir }()

	// Empty session id → "default" key.
	brainUpdateState("", map[string]any{"x": 1})

	expectedPath := dir + "/brain-hook-state-default.json"
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("expected state file at %s", expectedPath)
	}
}

func TestBrainUpdateState_WithConvId(t *testing.T) {
	dir := t.TempDir()
	origDir := brainStateDir
	brainStateDir = dir
	defer func() { brainStateDir = origDir }()

	convId := "my-conv-id"
	brainUpdateState("sess-1", map[string]any{
		"conversationId": &convId,
	})

	path := brainStateFile("sess-1")
	data, _ := os.ReadFile(path)
	stateStr := string(data)
	if !strings.Contains(stateStr, `"my-conv-id"`) {
		t.Errorf("conversationId string not present when non-empty:\n%s", stateStr)
	}
}

// ---------- cmdBrainRegister always prints ack ----------

func TestCmdBrainRegister_AlwaysPrintsAck(t *testing.T) {
	dir := t.TempDir()
	origDir := brainStateDir
	origLog := brainLogPath
	brainStateDir = dir
	brainLogPath = dir + "/brain-hook.log"
	defer func() {
		brainStateDir = origDir
		brainLogPath = origLog
	}()

	// Feed minimal stdin (no transcript path so convId will be empty).
	stdinJSON := `{"session_id":"test-sess","transcript_path":""}`
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	_, _ = w.WriteString(stdinJSON)
	_ = w.Close()
	defer func() { os.Stdin = oldStdin }()

	// Capture stdout.
	oldStdout := os.Stdout
	ro, wo, _ := os.Pipe()
	os.Stdout = wo

	// Brain service will not be available in test env — that's fine.
	cmdBrainRegister()

	_ = wo.Close()
	os.Stdout = oldStdout

	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, err := ro.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	_ = ro.Close()

	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected SessionStart ack on stdout, got nothing")
	}

	var ack map[string]any
	if err := json.Unmarshal([]byte(out), &ack); err != nil {
		t.Fatalf("ack is not valid JSON: %v\noutput: %s", err, out)
	}
	hso, ok := ack["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("hookSpecificOutput missing or wrong type: %v", ack)
	}
	if hso["hookEventName"] != "SessionStart" {
		t.Errorf("hookEventName: got %v want SessionStart", hso["hookEventName"])
	}
}
