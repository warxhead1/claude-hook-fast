// cmd_flow.go — Bash-command classification shared by cmd_rules.go (the
// live "rules" hook records rec["class"]/rec["cmd_120"] via classifyTool /
// first120). Originally paired with a "flow" PostToolUse subcommand that
// logged every tool call to ~/.claude/hooks/log/flow/YYYY-MM-DD.jsonl;
// that subcommand was removed 2026-08-30 (superseded by trace.go, never
// wired in settings.json, log dir last written June 13) but these two
// helpers are still load-bearing for cmd_rules.go and stay.

package main

import (
	"regexp"
	"strings"
)

// Bash classification regexes (applied to the first token of .command).
// Ordered: most-specific first.
var (
	reInstrument = regexp.MustCompile(`(?i)^(shadergen|silo_tester)`)
	reLifecycle  = regexp.MustCompile(`(?i)^(shadergen\s+(start|stop|reload|reset))|^silo_tester\s+\w+`)
	reSourceRead = regexp.MustCompile(`^(grep|rg|cat|sed|awk|head|tail|find|ls|wc|bat|fd)\b`)
	reCargo      = regexp.MustCompile(`\bcargo\s+(build|check|run)\b`)
	reSleepPoll  = regexp.MustCompile(`\b(sleep|until\s|while\s.*do)\b`)
	reTsdlSync   = regexp.MustCompile(`\btsdl_sync\b`)
	reGit        = regexp.MustCompile(`\bgit\b`)
	reGh         = regexp.MustCompile(`\bgh\b`)
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

// first120 returns the first 120 UTF-8 characters (not bytes) of s.
func first120(s string) string {
	runes := []rune(s)
	if len(runes) <= 120 {
		return s
	}
	return string(runes[:120])
}
