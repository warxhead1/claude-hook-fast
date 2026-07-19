package hookio

import (
	"os"
	"syscall"
	"testing"
)

func TestExtractConversationID(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/tmp/conversations/conversation-abc123.json", "conversation-abc123"},
		{"/tmp/conversations/conversation-test.json", "conversation-test"},
		{"/home/user/.claude/conversations/conversation-foo.json", "conversation-foo"},
		{"", ""},
		{"/tmp/noconversation.json", ""},
	}
	for _, tt := range tests {
		got := ExtractConversationID(tt.path)
		if got != tt.expected {
			t.Errorf("ExtractConversationID(%q) = %q, want %q", tt.path, got, tt.expected)
		}
	}
}

func TestIsSubagentTranscript(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// fresh dispatch: the /tasks/<id>.output symlink form
		{"/tmp/claude-1000/-home-eric-projects-tengine/S/tasks/a1.output", true},
		// resumed subagent: harness passes the RESOLVED symlink target —
		// this is the case that used to fail-open to lead treatment
		{"/home/eric/.claude/projects/-home-eric-projects-tengine/S/subagents/agent-a1.jsonl", true},
		// lead session: transcript lives at the project root, no marker dir
		{"/home/eric/.claude/projects/-home-eric-projects-tengine/839efa25.jsonl", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsSubagentTranscript(tt.path); got != tt.want {
			t.Errorf("IsSubagentTranscript(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestReadHookInputInvalid(t *testing.T) {
	_, ok := ReadHookInput()
	if ok {
		t.Error("ReadHookInput should return false for empty stdin")
	}
}

// The harness (Node child_process) hands hooks stdin as a SOCKETPAIR, not a
// pipe. os.ReadFile("/dev/stdin") re-opens fd 0 via /proc/self/fd/0, which
// fails with ENXIO on sockets — that made every guard fail-open in production
// (2026-07-17..19) while pipe-fed tests stayed green. This test feeds stdin
// from a real socketpair to pin the direct-read behavior.
func TestReadHookInputSocketpairStdin(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	reader := os.NewFile(uintptr(fds[0]), "sock-read")
	writer := os.NewFile(uintptr(fds[1]), "sock-write")
	defer reader.Close()

	payload := `{"tool_name":"Edit","tool_input":{"file_path":"/x/y.rs"},` +
		`"transcript_path":"/tmp/claude/t/tasks/a1.output","cwd":"/x"}`
	if _, err := writer.WriteString(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	writer.Close() // EOF for the reader side

	origStdin := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = origStdin }()

	input, ok := ReadHookInput()
	if !ok {
		t.Fatal("ReadHookInput failed on socketpair stdin — the production fail-open regression")
	}
	if input.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want Edit", input.ToolName)
	}
	if input.TranscriptPath != "/tmp/claude/t/tasks/a1.output" {
		t.Errorf("TranscriptPath = %q", input.TranscriptPath)
	}
}
