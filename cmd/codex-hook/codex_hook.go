// Codex hook integration for hearth-loom.
//
// This CLI acts as a hook script for the Codex CLI. Codex invokes it with
// hook event JSON on stdin and a subcommand (session-start | stop | pulse).
// Events are published to nervous-bus via `nervous publish` CLI.
//
// To configure Codex to use this hook, add the following to ~/.codex/config.toml:
//
//	[[hooks.PostToolUse]]
//	matcher = "Bash|Edit|Write|Read|Grep|Glob"
//	[[hooks.PostToolUse.hooks]]
//	type = "command"
//	command = "codex-hook pulse"
//
//	[[hooks.PreToolUse]]
//	matcher = "Bash|Edit|Write|Read|Grep|Glob"
//	[[hooks.PreToolUse.hooks]]
//	type = "command"
//	command = "codex-hook pre-tool"
//
//	[[hooks.PermissionRequest]]
//	matcher = "Bash"
//	[[hooks.PermissionRequest.hooks]]
//	type = "command"
//	command = "codex-hook permission-request"
//
//	[[hooks.SessionStart]]
//	matcher = "startup|resume|clear"
//	[[hooks.SessionStart.hooks]]
//	type = "command"
//	command = "codex-hook session-start"
//
//	[[hooks.Stop]]
//	[[hooks.Stop.hooks]]
//	type = "command"
//	command = "codex-hook stop"
//
// Hook stdin contract (per Codex docs):
//
//	session-start:  {session_id, cwd, hook_event_name, model, turn_id, source, permission_mode}
//	pre-tool:       {session_id, cwd, hook_event_name, tool_name, tool_input, tool_use_id, turn_id, permission_mode}
//	permission-req: {session_id, cwd, hook_event_name, tool_name, tool_input, permission_mode}
//	post-tool-use:  {session_id, cwd, hook_event_name, tool_name, tool_input, tool_use_id, turn_id, permission_mode}
//	stop:          {session_id, hook_event_name}
//
// Output contract:
//
//	PreToolUse / PermissionRequest / PostToolUse: exit 0 to allow. exit 2 + stderr to block.
//	SessionStart / Stop: exit 0, optional JSON output with systemMessage
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

const (
	chanAgentActivity   = "bus.agent.activity.v1"
	chanAgentSession    = "agent.session.v1"
	maxHookPayloadBytes = 1024 * 1024
)

type hookPayload struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd,omitempty"`
	Model         string `json:"model,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	// ToolInput is the authoritative Codex hook field. Input is retained for
	// compatibility with older locally generated hook payloads.
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

var secretArgPattern = regexp.MustCompile(`(?i)(--?(?:api[-_]?key|token|secret|password|authorization|credential|private[-_]?key)|(?:api[-_]?key|token|secret|password|authorization|credential|private[-_]?key))(\s+|=)(?:"[^"]*"|'[^']*'|[^\s]+)`)

func (p *hookPayload) effectiveInput() json.RawMessage {
	if p == nil {
		return nil
	}
	if len(p.ToolInput) > 0 && string(p.ToolInput) != "null" {
		return p.ToolInput
	}
	return p.Input
}

func newCodexHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex-hook",
		Short: "Act as a Codex hook script, dispatching hook events to hearth-loom",
	}
	cmd.AddCommand(
		newCodexHookSessionStartCmd(),
		newCodexHookStopCmd(),
		newCodexHookPulseCmd(),
		newCodexHookPreToolCmd(),
		newCodexHookPermissionRequestCmd(),
		newCodexHookPublishWorkerCmd(),
	)
	return cmd
}

// newCodexHookPublishWorkerCmd registers the detached bus-publish worker
// spawned by nbuspub.PublishAsync. Hidden — never invoked by Codex.
func newCodexHookPublishWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    nbuspub.WorkerSubcommand + " <channel> <source> <payload>",
		Hidden: true,
		Args:   cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			nbuspub.WorkerMain(args)
			return nil
		},
	}
}

func readPayload() (*hookPayload, error) {
	return readPayloadFrom(os.Stdin)
}

func readPayloadFrom(r io.Reader) (*hookPayload, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxHookPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHookPayloadBytes {
		return nil, fmt.Errorf("hook payload exceeds %d bytes", maxHookPayloadBytes)
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// detectAgentKind returns the CLI variant invoking this hook.
// Priority: codex-cli (no AI_AGENT env) → claude-code-minimax (minimax base URL) → claude-code.
func detectAgentKind() string {
	if os.Getenv("AI_AGENT") == "" {
		return "codex-cli"
	}
	if strings.Contains(os.Getenv("ANTHROPIC_BASE_URL"), "minimax.io") {
		return "claude-code-minimax"
	}
	return "claude-code"
}

// activityEvent builds+publishes a bus.agent.activity.v1 envelope. toolName
// and toolSummary are attached when non-empty — pulse/pre-tool thread the
// codex tool invocation through so hearth's activity feed carries what the
// agent is doing (mirroring the Claude PostToolUse tool_name/tool_summary
// enrichment). Lifecycle callers (session-start/stop) pass "".
//
// Codex hook stdin (session_id/cwd/model/tool_name/input) carries NO token
// usage — unlike Claude's transcript-tail read — so token counts are not
// emitted here; that enrichment lives hearth-side where codex rollout parsing
// (codex_jsonl) already exists.
func activityEvent(sessionID, cwd, model, nbEvent, toolName, toolSummary string) {
	can, wt := project.Resolve(cwd)
	kind := detectAgentKind()
	env := map[string]any{
		"ts":           time.Now().UTC().Format(time.RFC3339Nano),
		"event":        nbEvent,
		"agent":        "codex",
		"agent_kind":   kind,
		"agent_id":     sessionID,
		"session_id":   sessionID,
		"project":      can,
		"worktree":     wt,
		"display":      project.Display(can, wt),
		"cwd":          cwd,
		"model":        model,
		"source_kind":  kind,
		"pane_id":      codexPaneID(),
		"session_name": os.Getenv("ZELLIJ_SESSION_NAME"),
	}
	if toolName != "" {
		env["tool_name"] = toolName
	}
	if toolSummary != "" {
		env["tool_summary"] = toolSummary
	}
	publishEnvelope(chanAgentActivity, env, can)
}

// codexPaneID captures the pane the codex hook fired in, preferring zellij's
// bare pane id (paired with session_name in the envelope) and falling back to
// the raw tmux "%N" pane. Adds the TMUX_PANE fallback that activityEvent/
// sessionEvent previously lacked (X1 parity) — without it, tmux-only sessions
// emitted an empty pane_id and could not be pane-bound by hearth.
func codexPaneID() string {
	if p := os.Getenv("ZELLIJ_PANE_ID"); p != "" {
		return p
	}
	return os.Getenv("TMUX_PANE")
}

type toolInputDetails struct {
	Primary        string
	CommandPreview string
	Description    string
	Justification  string
	PromptText     string
}

func codexToolSummary(p *hookPayload) string {
	return toolInputDetailsFor(p).Primary
}

func toolInputDetailsFor(p *hookPayload) toolInputDetails {
	if p == nil || len(p.effectiveInput()) == 0 {
		return toolInputDetails{}
	}
	var value any
	if err := json.Unmarshal(p.effectiveInput(), &value); err != nil {
		return toolInputDetails{}
	}
	d := toolInputDetails{PromptText: boundedJSON(redactValue(value), 200)}
	if m, ok := value.(map[string]any); ok {
		d.Description, _ = m["description"].(string)
		d.Justification, _ = m["justification"].(string)
		if command, found := m["command"]; found {
			d.CommandPreview = boundedValue(command, 200)
			d.Primary = d.CommandPreview
		} else if action, _ := m["action"].(string); action != "" {
			d.Primary = truncate(redactString(action), 200)
		}
	}
	if d.Primary == "" {
		d.Primary = boundedValue(value, 200)
	}
	d.Description = truncate(redactString(d.Description), 200)
	d.Justification = truncate(redactString(d.Justification), 200)
	return d
}

func boundedValue(value any, max int) string {
	switch v := value.(type) {
	case string:
		return truncate(redactString(v), max)
	case []any:
		parts := make([]string, 0, len(v))
		for _, part := range v {
			parts = append(parts, boundedValue(part, max))
		}
		return truncate(strings.Join(parts, " "), max)
	default:
		return boundedJSON(redactValue(v), max)
	}
}

func boundedJSON(value any, max int) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return truncate(string(b), max)
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if isSecretKey(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = redactValue(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactValue(item)
		}
		return out
	case string:
		return redactString(v)
	default:
		return v
	}
}

func isSecretKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") || strings.Contains(key, "secret") ||
		strings.Contains(key, "password") || strings.Contains(key, "credential") ||
		strings.Contains(key, "authorization") || strings.Contains(key, "apikey") ||
		strings.Contains(key, "api_key") || strings.Contains(key, "api-key") ||
		strings.Contains(key, "private_key") || strings.Contains(key, "private-key")
}

func redactString(value string) string {
	return secretArgPattern.ReplaceAllString(value, "$1$2[REDACTED]")
}

func sessionEvent(sessionID, cwd, model, nbEvent string) {
	can, wt := project.Resolve(cwd)
	kind := detectAgentKind()
	env := map[string]any{
		"ts":           time.Now().UTC().Format(time.RFC3339Nano),
		"event":        nbEvent,
		"agent":        "codex",
		"agent_kind":   kind,
		"agent_id":     sessionID,
		"session_id":   sessionID,
		"project":      can,
		"worktree":     wt,
		"display":      project.Display(can, wt),
		"cwd":          cwd,
		"model":        model,
		"source_kind":  kind,
		"pane_id":      codexPaneID(),
		"session_name": os.Getenv("ZELLIJ_SESSION_NAME"),
	}
	publishEnvelope(chanAgentSession, env, can)
}

func publishEnvelope(channel string, env map[string]any, sourceProject string) {
	for k, v := range env {
		switch vv := v.(type) {
		case string:
			if vv == "" {
				delete(env, k)
			}
		case nil:
			delete(env, k)
		}
	}
	body, err := json.Marshal(env)
	if err != nil {
		return
	}
	// Detached publish worker (nbuspub.PublishAsync): Codex bounds these
	// hooks at 2s, and the synchronous nervous round-trip (3-4 python
	// spawns + redis) routinely exceeded that under load — every tool
	// call surfaced "hook timed out after 2s". The hook now pays only
	// fork+exec; the worker keeps the bounded publish + hooklog.
	_ = nbuspub.PublishAsync(channel, nbuspub.SourceTag(sourceProject), string(body))
}

func newCodexHookSessionStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "session-start",
		Short: "Handle Codex SessionStart hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPayload()
			if err != nil || p == nil || p.SessionID == "" {
				return nil
			}
			activityEvent(p.SessionID, p.CWD, p.Model, "started", "", "")
			sessionEvent(p.SessionID, p.CWD, p.Model, "started")
			return nil
		},
	}
}

func newCodexHookStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Handle Codex Stop hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPayload()
			if err != nil || p == nil || p.SessionID == "" {
				return nil
			}
			activityEvent(p.SessionID, p.CWD, "", "ended", "", "")
			sessionEvent(p.SessionID, p.CWD, "", "ended")
			return nil
		},
	}
}

func newCodexHookPulseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pulse",
		Short: "Handle Codex PostToolUse hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPayload()
			if err == nil && p != nil && p.SessionID != "" {
				activityEvent(p.SessionID, p.CWD, "", "heartbeat", p.ToolName, codexToolSummary(p))
			}
			return nil
		},
	}
}

func newCodexHookPreToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pre-tool",
		Short: "Handle Codex PreToolUse hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPayload()
			if err == nil && p != nil && p.SessionID != "" {
				activityEvent(p.SessionID, p.CWD, "", "heartbeat", p.ToolName, codexToolSummary(p))
			}
			return nil
		},
	}
}

func newCodexHookPermissionRequestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "permission-request",
		Short: "Handle Codex PermissionRequest hook — publishes permission card to phone/watch",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPayload()
			if err != nil || p == nil || p.SessionID == "" {
				return nil
			}
			can, wt := project.Resolve(p.CWD)
			paneID := os.Getenv("ZELLIJ_PANE_ID")
			if paneID == "" {
				paneID = os.Getenv("TMUX_PANE")
			}
			corrID := newUUID()

			details := toolInputDetailsFor(p)
			toolSummary := p.ToolName
			if details.Primary != "" {
				toolSummary += ": " + truncate(details.Primary, 120)
			}

			now := time.Now().UTC()
			expires := now.Add(5 * time.Minute)
			env := map[string]any{
				"correlation_id":  corrID,
				"session_id":      p.SessionID,
				"pane_id":         paneID,
				"project":         can,
				"project_short":   filepath.Base(can),
				"worktree":        wt,
				"tool_name":       p.ToolName,
				"tool_summary":    toolSummary,
				"prompt_text":     details.PromptText,
				"command_preview": details.CommandPreview,
				"description":     details.Description,
				"justification":   details.Justification,
				// Schema bus.hearth.session.permission.requested.v1 requires options as
				// structured objects {label, is_default, checked} — emitting bare strings
				// makes redis-mirror dead-letter the event ("'No' is not of type 'object'")
				// so it never reaches the consumer and no card is ever shown on phone/watch.
				// Mirrors the corrected Claude hook (claude-hooks/cmd_nbus.go).
				"options": []map[string]any{
					{"label": "Yes", "is_default": true, "checked": false},
					{"label": "Yes, and don't ask again for this session", "is_default": false, "checked": false},
					{"label": "No", "is_default": false, "checked": false},
				},
				"deep_link":  "session://" + p.SessionID,
				"ts":         now.Format(time.RFC3339Nano),
				"expires_at": expires.Format(time.RFC3339Nano),
			}
			publishEnvelope("bus.hearth.session.permission.requested.v1", env, can)
			return nil
		},
	}
}

// newUUID generates a random UUID string. Uses crypto/rand via os.ReadFile
// on /proc/sys/kernel/random/uuid when available, falling back to time-based.
func newUUID() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/uuid"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
