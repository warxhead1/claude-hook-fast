package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- extractConversationUUID ----------

func TestExtractConversationUUID_Valid(t *testing.T) {
	path := "/home/user/.claude/projects/my-project/a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl"
	got := extractConversationUUID(path)
	want := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestExtractConversationUUID_NonUUID(t *testing.T) {
	path := "/home/user/.claude/projects/my-project/not-a-uuid.jsonl"
	got := extractConversationUUID(path)
	if got != "" {
		t.Errorf("expected empty string for non-UUID basename, got %q", got)
	}
}

func TestExtractConversationUUID_Empty(t *testing.T) {
	got := extractConversationUUID("")
	if got != "" {
		t.Errorf("expected empty string for empty path, got %q", got)
	}
}

func TestExtractConversationUUID_NoExtension(t *testing.T) {
	// Basename without .jsonl that happens to match UUID pattern
	path := "/tmp/a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	// Without .jsonl the basename IS the UUID directly
	got := extractConversationUUID(path)
	want := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestExtractConversationUUID_UpperCase(t *testing.T) {
	// UUID with uppercase — should NOT match (regex is [a-f0-9] lowercase only, matching JS)
	path := "/tmp/A1B2C3D4-E5F6-7890-ABCD-EF1234567890.jsonl"
	got := extractConversationUUID(path)
	if got != "" {
		t.Errorf("uppercase UUID should return empty, got %q", got)
	}
}

// ---------- decideNewConversation ----------

func makeConvMap(convId string, lastActivityMs int64) map[string]siConvEntry {
	return map[string]siConvEntry{
		convId: {
			ConversationID: convId,
			LastActivity:   lastActivityMs,
		},
	}
}

const testUUID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

func TestDecideNewConversation_Clear(t *testing.T) {
	now := int64(1_000_000_000_000)
	existing := makeConvMap(testUUID, now-1000)
	if !decideNewConversation(existing, testUUID, "clear", now) {
		t.Error("source=clear should always be new")
	}
}

func TestDecideNewConversation_Startup(t *testing.T) {
	now := int64(1_000_000_000_000)
	existing := makeConvMap(testUUID, now-1000)
	if !decideNewConversation(existing, testUUID, "startup", now) {
		t.Error("source=startup should always be new")
	}
}

func TestDecideNewConversation_MissingConv(t *testing.T) {
	now := int64(1_000_000_000_000)
	existing := map[string]siConvEntry{} // empty map
	if !decideNewConversation(existing, testUUID, "resume", now) {
		t.Error("missing conversation should be new")
	}
}

func TestDecideNewConversation_EmptyConvId(t *testing.T) {
	now := int64(1_000_000_000_000)
	existing := makeConvMap(testUUID, now-1000)
	if !decideNewConversation(existing, "", "resume", now) {
		t.Error("empty convId should be new")
	}
}

func TestDecideNewConversation_RecentResume_Continuation(t *testing.T) {
	now := int64(1_000_000_000_000)
	// Last activity 1 hour ago — within 24 h window → continuation
	existing := makeConvMap(testUUID, now-3_600_000)
	if decideNewConversation(existing, testUUID, "resume", now) {
		t.Error("recent resume (1h ago) should be continuation, not new")
	}
}

func TestDecideNewConversation_OldResume_New(t *testing.T) {
	now := int64(1_000_000_000_000)
	// Last activity 25 hours ago → treated as new
	const msIn25h = 25 * 60 * 60 * 1000
	existing := makeConvMap(testUUID, now-msIn25h)
	if !decideNewConversation(existing, testUUID, "resume", now) {
		t.Error("resume > 24h ago should be new")
	}
}

func TestDecideNewConversation_RecentCompact_Continuation(t *testing.T) {
	now := int64(1_000_000_000_000)
	existing := makeConvMap(testUUID, now-600_000) // 10 min ago
	if decideNewConversation(existing, testUUID, "compact", now) {
		t.Error("recent compact should be continuation")
	}
}

// ---------- siConvEntry JSON round-trip ----------

func TestConvEntryJSONShape(t *testing.T) {
	entry := siConvEntry{
		ConversationID:        "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		ConversationStartTime: 1781220215644,
		CurrentWindow:         "1",
		TotalOperations:       0,
		LastActivity:          1781220215644,
		RecentSessionIDs:      []string{"sess-abc"},
		WindowsUsed:           []string{"1"},
		Created:               1781220215644,
		LastUpdate:            1781220215644,
		TmuxSession:           "cc-tengine-claude",
	}

	b, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	s := string(b)

	// Verify required field names are present and in the documented order.
	for _, field := range []string{
		`"conversationId"`,
		`"conversation_start_time"`,
		`"current_window"`,
		`"total_operations"`,
		`"last_activity"`,
		`"recent_session_ids"`,
		`"windows_used"`,
		`"created"`,
		`"last_update"`,
		`"tmux_session"`,
	} {
		if !strings.Contains(s, field) {
			t.Errorf("JSON missing field %s", field)
		}
	}

	// Field order: conversationId must appear before tmux_session.
	idPos := strings.Index(s, `"conversationId"`)
	sessPos := strings.Index(s, `"tmux_session"`)
	if idPos < 0 || sessPos < 0 || idPos >= sessPos {
		t.Errorf("field order unexpected: conversationId at %d, tmux_session at %d", idPos, sessPos)
	}

	// Round-trip: decode back and check values.
	var decoded siConvEntry
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if decoded.ConversationID != entry.ConversationID {
		t.Errorf("conversationId mismatch: got %q", decoded.ConversationID)
	}
	if decoded.TmuxSession != "cc-tengine-claude" {
		t.Errorf("tmux_session mismatch: got %q", decoded.TmuxSession)
	}
}

// ---------- isProcessAlive ----------

func TestIsProcessAlive_Self(t *testing.T) {
	if !isProcessAlive(os.Getpid()) {
		t.Error("our own process should be alive")
	}
}

func TestIsProcessAlive_ImpossiblePID(t *testing.T) {
	// PID 2147483647 (max int32) is virtually guaranteed not to exist.
	if isProcessAlive(2_147_483_647) {
		t.Error("impossible PID should not be alive")
	}
}

// ---------- file I/O helpers with temp paths ----------

func TestLoadSaveConversations_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	old := siConversationsFile
	siConversationsFile = filepath.Join(tmp, "conversations.json")
	defer func() { siConversationsFile = old }()

	m := map[string]siConvEntry{
		testUUID: {
			ConversationID:   testUUID,
			LastActivity:     12345,
			RecentSessionIDs: []string{"s1"},
			WindowsUsed:      []string{"3"},
		},
	}
	saveConversations(m)
	loaded := loadConversations()
	if loaded[testUUID].LastActivity != 12345 {
		t.Errorf("round-trip failed: got last_activity=%d", loaded[testUUID].LastActivity)
	}
}

func TestLoadConversations_MissingFile(t *testing.T) {
	old := siConversationsFile
	siConversationsFile = "/tmp/this-file-does-not-exist-ever.json"
	defer func() { siConversationsFile = old }()
	m := loadConversations()
	if len(m) != 0 {
		t.Errorf("expected empty map for missing file, got %d entries", len(m))
	}
}

func TestUpdateConversation_New(t *testing.T) {
	tmp := t.TempDir()
	old1 := siConversationsFile
	old2 := siWindowConvsFile
	siConversationsFile = filepath.Join(tmp, "conversations.json")
	siWindowConvsFile = filepath.Join(tmp, "window-convs.json")
	defer func() {
		siConversationsFile = old1
		siWindowConvsFile = old2
	}()

	entry := updateConversation(testUUID, "3", "sess-xyz", true)
	if entry.ConversationID != testUUID {
		t.Errorf("conversationId mismatch: %q", entry.ConversationID)
	}
	if entry.CurrentWindow != "3" {
		t.Errorf("current_window mismatch: %q", entry.CurrentWindow)
	}
	if len(entry.RecentSessionIDs) != 1 || entry.RecentSessionIDs[0] != "sess-xyz" {
		t.Errorf("recent_session_ids mismatch: %v", entry.RecentSessionIDs)
	}

	// Verify window mapping was written
	wm := loadWindowConvs()
	if wm["3"] != testUUID {
		t.Errorf("window mapping missing: %v", wm)
	}
}

func TestUpdateConversation_Continuation_DeduplicatesSessionIDs(t *testing.T) {
	tmp := t.TempDir()
	old1 := siConversationsFile
	old2 := siWindowConvsFile
	siConversationsFile = filepath.Join(tmp, "conversations.json")
	siWindowConvsFile = filepath.Join(tmp, "window-convs.json")
	defer func() {
		siConversationsFile = old1
		siWindowConvsFile = old2
	}()

	// Seed with a conversation that already has sess-1.
	initial := map[string]siConvEntry{
		testUUID: {
			ConversationID:        testUUID,
			ConversationStartTime: 1000,
			CurrentWindow:         "1",
			RecentSessionIDs:      []string{"sess-1"},
			WindowsUsed:           []string{"1"},
			LastActivity:          1000,
			LastUpdate:            1000,
		},
	}
	saveConversations(initial)

	// Continue with a new session ID.
	entry := updateConversation(testUUID, "1", "sess-2", false)
	if !containsStr(entry.RecentSessionIDs, "sess-2") {
		t.Errorf("new session id not prepended: %v", entry.RecentSessionIDs)
	}
	if !containsStr(entry.RecentSessionIDs, "sess-1") {
		t.Errorf("old session id should be retained: %v", entry.RecentSessionIDs)
	}
	// Prepend means sess-2 comes first
	if entry.RecentSessionIDs[0] != "sess-2" {
		t.Errorf("sess-2 should be first: %v", entry.RecentSessionIDs)
	}
}

func TestUpdateConversation_KeepsAtMost5SessionIDs(t *testing.T) {
	tmp := t.TempDir()
	old1 := siConversationsFile
	old2 := siWindowConvsFile
	siConversationsFile = filepath.Join(tmp, "conversations.json")
	siWindowConvsFile = filepath.Join(tmp, "window-convs.json")
	defer func() {
		siConversationsFile = old1
		siWindowConvsFile = old2
	}()

	initial := map[string]siConvEntry{
		testUUID: {
			ConversationID:   testUUID,
			RecentSessionIDs: []string{"s1", "s2", "s3", "s4", "s5"},
			WindowsUsed:      []string{"1"},
		},
	}
	saveConversations(initial)

	entry := updateConversation(testUUID, "1", "s6", false)
	if len(entry.RecentSessionIDs) > 5 {
		t.Errorf("should keep at most 5 session IDs, got %d: %v", len(entry.RecentSessionIDs), entry.RecentSessionIDs)
	}
	if entry.RecentSessionIDs[0] != "s6" {
		t.Errorf("s6 should be first, got %v", entry.RecentSessionIDs)
	}
}

// ---------- formatDuration ----------

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{500, "500ms"},
		{1000, "1s"},
		{5500, "5s"},
		{60000, "1m 0s"},
		{90000, "1m 30s"},
		{3661000, "61m 1s"},
	}
	for _, c := range cases {
		got := formatDuration(c.ms)
		if got != c.want {
			t.Errorf("formatDuration(%d) = %q want %q", c.ms, got, c.want)
		}
	}
}
