package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

var publishAsync = nbuspub.PublishAsync // deterministic test seam

// detectAgentKind always reports "codex-cli": this binary is only ever
// invoked from codex's own [[hooks.*]] wiring in config.toml (see
// buildEnvelope's hardcoded "agent": "codex"), so every session it sees
// IS a real Codex CLI session.
//
// It used to branch on the AI_AGENT / ANTHROPIC_BASE_URL env vars to
// (mis)detect "claude-code" and "claude-code-minimax" flavors. That
// heuristic is retired: nervous-bus issue #34 found it corrupting the
// coverage metrics — AI_AGENT is an environment variable that inherits
// down any process tree, so a real `codex exec` launched from inside an
// unrelated AI_AGENT-flagged ancestor process (measured: a Gemini/
// Antigravity "council" sandbox at /tmp/council-sandbox-desktop) got
// mislabeled agent_kind="claude-code" for its whole lifetime even though
// it was genuine Codex. 246/257 (95.7%) of runs.db's "claude-code" bucket
// had an empty tool_histogram as a direct result — not because those
// sessions used no tools, but because the label routed them out of the
// codex-cli population whose matcher/coverage story is actually healthy
// (98.4% coverage in September 2026). MiniMax's own Claude-Code-in-a-
// sandbox path is a fully separate producer (cmmx-hook, wired by
// claude-minimax-sandboxed with its own hooks.json) that already tags
// "claude-code-minimax" correctly; this binary never needs to guess it.
func detectAgentKind() string { return "codex-cli" }

func isSubagent(p *hookPayload) bool {
	if p == nil {
		return false
	}
	t := strings.ToLower(p.AgentType)
	transcript := strings.ReplaceAll(p.TranscriptPath+"/"+p.AgentTranscriptPath, "\\", "/")
	return (p.AgentID != "" && p.AgentID != p.SessionID) || strings.Contains(t, "subagent") || strings.Contains(t, "child") || strings.Contains(transcript, "/subagents/")
}

func qualifiedPane() (raw, qualified string) {
	if pane := os.Getenv("ZELLIJ_PANE_ID"); pane != "" {
		session := os.Getenv("ZELLIJ_SESSION_NAME")
		if session == "" {
			session = "unknown"
		}
		return pane, "zellij:" + session + ":" + pane
	}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		return pane, "tmux:" + pane
	}
	return "", ""
}

// codexPaneID preserves the legacy helper while emitted events carry pane_binding.
func codexPaneID() string { raw, _ := qualifiedPane(); return raw }

func stableEventID(p *hookPayload, event string) string {
	if p == nil {
		return ""
	}
	s := strings.Join([]string{event, p.SessionID, p.AgentID, p.TurnID, p.ToolUseID, p.Source, p.Reason, p.Trigger}, "\x1f")
	h := sha256.Sum256([]byte(s))
	return "codex-" + hex.EncodeToString(h[:16])
}

func eventID(p *hookPayload, wireEvent, action string, now time.Time) string {
	if action != "lease_refresh" {
		return stableEventID(p, wireEvent+":"+action)
	}
	bucket := now.UTC().Unix() / int64(leasePeriod/time.Second)
	copy := *p
	copy.Trigger = copy.Trigger + ":lease:" + strconv.FormatInt(bucket, 10)
	return stableEventID(&copy, wireEvent+":"+action)
}

func wireEvent(action string, lifecycle bool) string {
	if lifecycle {
		switch action {
		case "started":
			return "started"
		case "turn_end":
			return "turn_end"
		case "compact_pre", "compact_post":
			return "compact"
		case "ended":
			return "ended"
		default:
			return "updated"
		}
	}
	switch action {
	case "tool_pre":
		return "tool_call"
	case "tool_post":
		return "tool_return"
	case "permission_requested":
		return "permission_requested"
	default:
		return "heartbeat"
	}
}

func bounded(s string, max int) string { return truncate(redactString(s), max) }

// buildEnvelope preserves identity fields but emits only bounded, redacted
// descriptions of tool input/response. Root session owns the pane; a child
// reports its own identity and root_session_id without claiming that pane.
func buildEnvelope(p *hookPayload, event string) (map[string]any, string) {
	can, wt := project.Resolve(p.CWD)
	agentID := p.AgentID
	if agentID == "" {
		agentID = p.SessionID
	}
	sub := isSubagent(p)
	env := map[string]any{
		"event_id": stableEventID(p, event), "ts": time.Now().UTC().Format(time.RFC3339Nano), "event": event,
		"event_sequence": bounded(p.TurnID, 160) + ":" + event,
		"agent":          "codex", "agent_kind": detectAgentKind(), "root_session_id": p.SessionID, "session_id": p.SessionID,
		"agent_id": agentID, "agent_type": p.AgentType, "is_subagent": sub,
		"project": can, "worktree": wt, "display": project.Display(can, wt), "cwd": bounded(p.CWD, 512),
		"model": bounded(p.Model, 120), "permission_mode": bounded(p.PermissionMode, 80), "turn_id": bounded(p.TurnID, 160),
		"hook_event_name": bounded(p.HookEventName, 80), "source": bounded(p.Source, 80), "reason": bounded(p.Reason, 160),
		"trigger": bounded(p.Trigger, 160), "transcript_path": bounded(p.TranscriptPath, 512),
		"agent_transcript_path": bounded(p.AgentTranscriptPath, 512), "tool_name": bounded(p.ToolName, 120), "tool_use_id": bounded(p.ToolUseID, 160),
		"stop_hook_active":   p.StopHookActive,
		"tool_input_present": len(p.effectiveInput()) > 0, "tool_response_present": len(p.ToolResponse) > 0,
	}
	if !sub {
		raw, binding := qualifiedPane()
		env["pane_id"] = raw
		env["pane_binding"] = binding
	}
	if details := toolInputDetailsFor(p); details.Primary != "" {
		env["tool_summary"] = details.Primary
		env["command_preview"] = details.CommandPreview
		env["description"] = details.Description
		env["justification"] = details.Justification
	}
	if p.Prompt != "" {
		env["prompt_summary"] = bounded(p.Prompt, 240)
	}
	if p.LastAssistantMessage != "" {
		env["last_assistant_summary"] = bounded(p.LastAssistantMessage, 240)
	}
	return compactEnvelope(env), can
}

func compactEnvelope(env map[string]any) map[string]any {
	for k, v := range env {
		if s, ok := v.(string); ok && s == "" {
			delete(env, k)
		}
	}
	return env
}

func publishEnvelope(channel string, env map[string]any, sourceProject string) {
	body, err := json.Marshal(compactEnvelope(env))
	if err != nil {
		hooklog.WriteEvent("codex_hook_marshal_failed", map[string]any{"hook_event": env["event"], "error": err.Error()})
		return
	}
	if err := publishAsync(channel, nbuspub.SourceTag(sourceProject), string(body)); err != nil {
		hooklog.WriteEvent("codex_hook_publish_failed", map[string]any{"hook_event": env["event"], "event_id": env["event_id"], "error": err.Error()})
	}
}

func emitHook(p *hookPayload, action string, lifecycle bool) {
	if p == nil || p.SessionID == "" {
		return
	}
	wire := wireEvent(action, lifecycle)
	env, projectName := buildEnvelope(p, wire)
	env["hook_action"] = action
	env["event_id"] = eventID(p, wire, action, leaseNow())
	if !allowPaneEmission(p, action) {
		delete(env, "pane_id")
		delete(env, "pane_binding")
	}
	channel := chanAgentActivity
	if lifecycle {
		channel = chanAgentSession
	}
	publishEnvelope(channel, env, projectName)
}
