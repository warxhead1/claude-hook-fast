package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

var publishAsync = nbuspub.PublishAsync // deterministic test seam

// detectAgentKind always reports "codex-cli": this binary only runs from
// codex's own [[hooks.*]] wiring. Env heuristics (AI_AGENT inherits down any
// process tree) mislabelled real codex sessions; MiniMax has its own producer.
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
		"agent_transcript_path": bounded(p.AgentTranscriptPath, 512), "tool_use_id": bounded(p.ToolUseID, 160),
		"stop_hook_active":   p.StopHookActive,
		"tool_input_present": len(p.effectiveInput()) > 0, "tool_response_present": len(p.ToolResponse) > 0,
	}
	// tool_name is omitted on heartbeats: a heartbeat carries no real tool
	// call, but historically emitted tool_name:"Bash" anyway (a stale value
	// left over from the last real call), which made reflex-recorder's
	// _infer_from_behavior count every heartbeat as a tool call — a June
	// tengine pane that only heartbeated for 15+ minutes was labelled
	// "abandoned" purely from that stale field (nervous-bus#35 §fix 6).
	if event != "heartbeat" {
		env["tool_name"] = bounded(p.ToolName, 120)
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
	if summary := gitOperationSummary(p); summary != "" {
		env["tool_response_summary"] = summary
	}
	if isErr, ok := codexToolIsError(p); ok && event == "tool_return" {
		env["tool_is_error"] = isErr
	}
	if p.Prompt != "" {
		env["prompt_summary"] = bounded(p.Prompt, 240)
	}
	if p.LastAssistantMessage != "" {
		env["last_assistant_summary"] = bounded(p.LastAssistantMessage, 240)
	}
	return compactEnvelope(env), can
}

// reGitCommitOrPush and reGhPrCreateOrMerge detect a resolving git/gh
// operation ANYWHERE in the full, untruncated command text — unlike
// toolInputDetailsFor's command_preview (payload.go boundedValue, 200
// chars), which is what the label.py fallback regex was limited to
// (nervous-bus#35 §fix 2: a `gofmt && go test <long pkg list> && git commit`
// loses its commit past char 200).
//
// Anchored to avoid label.py's own false-positive class (`\bgit\b.*\bcommit\b`
// matches `git diff-tree --no-commit-id`): "commit"/"push" must be the
// subcommand word immediately following "git" (optionally past a `-C <dir>`
// global flag), not merely present anywhere later in the line.
var (
	reGitCommitOrPush   = regexp.MustCompile(`\bgit\b(\s+-C\s+\S+)?\s+(commit|push)\b`)
	reGhPrCreateOrMerge = regexp.MustCompile(`\bgh\s+pr\s+(create|merge)\b`)
)

// gitOperationSummary scans every shell command in the tool call for a
// resolving git/gh operation and, if any is found, returns a JSON string
// shaped {"gitOperation":{"commit":{"kind":"committed"}, ...}} — the exact
// field name and nesting reflex-recorder's label.py._has_resolving_commit
// already looks for as its PRIMARY signal, inside tool_response_summary
// (adapters/reflex-recorder/label.py:489-508). Emitting it there needs no
// nervous-bus schema change (bus.agent.activity.v1.json's tool_response_summary
// is already a free-form string, additionalProperties:true) and no label.py
// change — codex-hook was simply never populating a field label.py already
// knew how to read.
func gitOperationSummary(p *hookPayload) string {
	op := map[string]any{}
	for _, cmd := range shellCommands(p) {
		if reGitCommitOrPush.MatchString(cmd) {
			for _, m := range reGitCommitOrPush.FindAllStringSubmatch(cmd, -1) {
				switch m[2] {
				case "commit":
					op["commit"] = map[string]any{"kind": "committed"}
				case "push":
					op["push"] = map[string]any{"kind": "pushed"}
				}
			}
		}
		if m := reGhPrCreateOrMerge.FindStringSubmatch(cmd); m != nil {
			switch m[1] {
			case "create":
				op["pr_create"] = map[string]any{"kind": "pr_created"}
			case "merge":
				op["pr_merge"] = map[string]any{"kind": "pr_merged"}
			}
		}
	}
	if len(op) == 0 {
		return ""
	}
	b, err := json.Marshal(map[string]any{"gitOperation": op})
	if err != nil {
		return ""
	}
	return string(b)
}

// codexToolIsError reports whether the tool call failed, from tool_response.
// ok is false when the codex runtime gives no signal to read at all — which,
// as of the current Codex CLI (verified live against openai/codex#34289,
// still open, most recently reconfirmed on 0.152.1/0.154.0 as of
// 2026-09-18), is EVERY call: PostToolUse's tool_response is a plain string
// (the model-facing output text), with no is_error/exit_code field, and a
// silent failure (`sh -c 'exit 3'`) is byte-identical to a silent success.
// The exit code exists internally (codex's own rollout items record it) but
// is stripped before the hook runs; upstream has three proposed fixes open
// and none shipped. Content heuristics on tool_response text are explicitly
// NOT an acceptable substitute (upstream issue thread): they false-positive
// on successful commands whose output merely contains error-like text (test
// logs, grep output) and cannot detect a quiet failure at all, so this
// function does not attempt one.
//
// What IS implemented: parsing tool_response as a JSON object for any of the
// fields codex might reasonably ship (is_error, exit_code, the proposed
// tool_exit_code, or a nested status.exit_code) — free, forward-compatible,
// and activates automatically the day upstream adds one, with zero further
// changes here. Until then this always returns ok=false and tool_is_error is
// correctly omitted rather than fabricated.
func codexToolIsError(p *hookPayload) (isError bool, ok bool) {
	if p == nil || len(p.ToolResponse) == 0 {
		return false, false
	}
	var v any
	if json.Unmarshal(p.ToolResponse, &v) != nil {
		return false, false
	}
	m, isObj := v.(map[string]any)
	if !isObj {
		return false, false // current codex shape: a plain string — no signal
	}
	if b, present := m["is_error"].(bool); present {
		return b, true
	}
	for _, key := range []string{"exit_code", "tool_exit_code"} {
		if n, present := asInt(m[key]); present {
			return n != 0, true
		}
	}
	if status, present := m["status"].(map[string]any); present {
		if n, present2 := asInt(status["exit_code"]); present2 {
			return n != 0, true
		}
	}
	return false, false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
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
