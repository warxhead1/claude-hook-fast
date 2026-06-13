package hookio

import (
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

func TestReadHookInputInvalid(t *testing.T) {
	_, ok := ReadHookInput()
	if ok {
		t.Error("ReadHookInput should return false for empty stdin")
	}
}
