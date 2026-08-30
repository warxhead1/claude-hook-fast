package tmux

import (
	"testing"
)

func TestFormatToolStatus(t *testing.T) {
	tests := []struct {
		toolName string
		want     string
	}{
		{"Bash", "⚡ bash"},
		{"Read", "📖 read"},
		{"Write", "✏️ write"},
		{"Edit", "✏️ edit"},
		{"Grep", "🔍 grep"},
		{"Glob", "📂 glob"},
		{"TodoWrite", "📝 todo"},
		{"Unknown", "⚡ unknown"},
	}
	for _, tt := range tests {
		got := FormatToolStatus(tt.toolName)
		if got != tt.want {
			t.Errorf("FormatToolStatus(%q) = %q, want %q", tt.toolName, got, tt.want)
		}
	}
}

func TestGetSessionID(t *testing.T) {
	id := GetSessionID()
	if id == "" {
		t.Error("GetSessionID should not return empty string")
	}
}
