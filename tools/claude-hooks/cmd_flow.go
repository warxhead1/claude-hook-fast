// cmd_flow.go — PostToolUse * hook that appends a single JSONL line to
// ~/.claude/hooks/log/flow/YYYY-MM-DD.jsonl for every tool call.
//
// Fields:
//
//	ts      — RFC3339Nano timestamp
//	sid     — session_id
//	agent   — AgentKind (host_subagent | host_claude_code)
//	tool    — tool_name
//	class   — classify(tool, command): see classifyTool
//	cmd_120 — first 120 chars of tool_input.command (Bash only, else "")
//
// Design constraints:
//   - Crash-proof: any parse or I/O failure → exit 0 silently.
//   - Allocation-light: one JSON marshal, one file open/write/close.
//   - No subprocess, no HTTP.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

// flowEntry is the JSONL record written per tool call.
type flowEntry struct {
	TS     string `json:"ts"`
	SID    string `json:"sid"`
	Agent  string `json:"agent"`
	Tool   string `json:"tool"`
	Class  string `json:"class"`
	Cmd120 string `json:"cmd_120,omitempty"`
}

// Bash classification regexes (applied to the first token of .command).
// Ordered: most-specific first.
var (
	reInstrument  = regexp.MustCompile(`(?i)^(shadergen|silo_tester)`)
	reLifecycle   = regexp.MustCompile(`(?i)^(shadergen\s+(start|stop|reload|reset))|^silo_tester\s+\w+`)
	reSourceRead  = regexp.MustCompile(`^(grep|rg|cat|sed|awk|head|tail|find|ls|wc|bat|fd)\b`)
	reCargo       = regexp.MustCompile(`\bcargo\s+(build|check|run)\b`)
	reSleepPoll   = regexp.MustCompile(`\b(sleep|until\s|while\s.*do)\b`)
	reTsdlSync    = regexp.MustCompile(`\btsdl_sync\b`)
	reGit         = regexp.MustCompile(`\bgit\b`)
	reGh          = regexp.MustCompile(`\bgh\b`)
)

// classifyTool returns a class string for the JSONL record.
// Only Bash commands get a content-based class; all other tools return
// their tool name directly.
func classifyTool(toolName, command string) string {
	if toolName != "Bash" {
		return toolName
	}
	cmd := strings.TrimSpace(command)
	switch {
	case reLifecycle.MatchString(cmd):
		return "lifecycle"
	case reInstrument.MatchString(cmd):
		return "instrument"
	case reSourceRead.MatchString(cmd):
		return "source-read"
	case reTsdlSync.MatchString(cmd):
		return "tsdl_sync"
	case reCargo.MatchString(cmd):
		return "cargo"
	case reSleepPoll.MatchString(cmd):
		return "sleep-poll"
	case reGit.MatchString(cmd):
		return "git"
	case reGh.MatchString(cmd):
		return "gh"
	default:
		return "other"
	}
}

// flowLogPath returns ~/.claude/hooks/log/flow/YYYY-MM-DD.jsonl.
func flowLogPath(now time.Time) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	dir := filepath.Join(home, ".claude", "hooks", "log", "flow")
	_ = os.MkdirAll(dir, 0o755) // best-effort; failure handled at open
	return filepath.Join(dir, now.Format("2006-01-02")+".jsonl")
}

// first120 returns the first 120 UTF-8 characters (not bytes) of s.
func first120(s string) string {
	runes := []rune(s)
	if len(runes) <= 120 {
		return s
	}
	return string(runes[:120])
}

// cmdFlowTelemetry is the "flow" subcommand entry point.
func cmdFlowTelemetry() {
	in, _ := readStdinForHook()
	if in.ToolName == "" {
		return
	}

	// Extract .command from tool_input (Bash only).
	var command string
	if in.ToolName == "Bash" && len(in.ToolInput) > 0 {
		var ti map[string]any
		if err := json.Unmarshal(in.ToolInput, &ti); err == nil {
			if c, ok := ti["command"].(string); ok {
				command = c
			}
		}
	}

	now := time.Now().UTC()
	entry := flowEntry{
		TS:     now.Format(time.RFC3339Nano),
		SID:    in.SessionID,
		Agent:  in.AgentKind(),
		Tool:   in.ToolName,
		Class:  classifyTool(in.ToolName, command),
		Cmd120: first120(command),
	}

	line, err := json.Marshal(entry)
	if err != nil {
		// Marshal failure is impossible for this struct, but guard anyway.
		hooklog.Writef("flow marshal error: %v", err)
		return
	}
	line = append(line, '\n')

	path := flowLogPath(now)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		hooklog.Writef("flow open %s: %v", path, err)
		return
	}
	_, _ = f.Write(line)
	_ = f.Close()
}
