// Codex hook integration for hearth-loom.
//
// This CLI acts as a hook script for the Codex CLI. Codex invokes it with
// hook event JSON on stdin and a subcommand (session-start | stop | pulse).
// Events are published to nervous-bus via `nervous publish` CLI.
//
// To configure Codex to use this hook, add the following to ~/.codex/config.toml:
//
//   [[hooks.PostToolUse]]
//   matcher = "Bash|Edit|Write|Read|Grep|Glob"
//   [[hooks.PostToolUse.hooks]]
//   type = "command"
//   command = "codex-hook pulse"
//
//   [[hooks.PreToolUse]]
//   matcher = "Bash|Edit|Write|Read|Grep|Glob"
//   [[hooks.PreToolUse.hooks]]
//   type = "command"
//   command = "codex-hook pre-tool"
//
//   [[hooks.PermissionRequest]]
//   matcher = "Bash"
//   [[hooks.PermissionRequest.hooks]]
//   type = "command"
//   command = "codex-hook permission-request"
//
//   [[hooks.SessionStart]]
//   matcher = "startup|resume|clear"
//   [[hooks.SessionStart.hooks]]
//   type = "command"
//   command = "codex-hook session-start"
//
//   [[hooks.Stop]]
//   [[hooks.Stop.hooks]]
//   type = "command"
//   command = "codex-hook stop"
//
// Hook stdin contract (per Codex docs):
//   session-start:  {session_id, cwd, hook_event_name, model, turn_id, source, permission_mode}
//   pre-tool:       {session_id, cwd, hook_event_name, tool_name, tool_input, tool_use_id, turn_id, permission_mode}
//   permission-req: {session_id, cwd, hook_event_name, tool_name, tool_input, permission_mode}
//   post-tool-use:  {session_id, cwd, hook_event_name, tool_name, tool_input, tool_use_id, turn_id, permission_mode}
//   stop:          {session_id, hook_event_name}
//
// Output contract:
//   PreToolUse / PermissionRequest / PostToolUse: exit 0 to allow. exit 2 + stderr to block.
//   SessionStart / Stop: exit 0, optional JSON output with systemMessage
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

const (
	chanAgentActivity = "bus.agent.activity.v1"
	chanAgentSession  = "agent.session.v1"
)

// PublishTimeout is retained for compatibility but no longer used for blocking;
// nervous publish runs fire-and-forget to avoid hitting the Claude Code 2s hook timeout.
const PublishTimeout = 2 * time.Second

type hookPayload struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd,omitempty"`
	Model         string `json:"model,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	Input         json.RawMessage `json:"input,omitempty"`
}

func SourceTag(project string) string {
	if project == "" {
		project = "unknown"
	}
	return "/claude-host/" + project
}

func Publish(channel, source, payload string) error {
	if _, err := exec.LookPath("nervous"); err != nil {
		return nil
	}
	// Fire-and-forget: don't block waiting for nervous publish to complete.
	// Claude Code kills hooks that run longer than 2s; nervous publish can
	// occasionally be slow, so we launch it async and return immediately.
	cmd := exec.Command("nervous", "publish", channel, payload)
	cmd.Env = childEnv(source)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // prevent zombie; result not needed
	return nil
}

func childEnv(source string) []string {
	env := os.Environ()
	out := env[:0]
	const prefix = "NERVOUS_SOURCE="
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	hasNoZellij := false
	const zk = "NERVOUS_NO_ZELLIJ="
	for _, kv := range out {
		if len(kv) >= len(zk) && kv[:len(zk)] == zk {
			hasNoZellij = true
			break
		}
	}
	if !hasNoZellij {
		out = append(out, "NERVOUS_NO_ZELLIJ=1")
	}
	out = append(out, prefix+source)
	return out
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
	)
	return cmd
}

func readPayload() (*hookPayload, error) {
	var p hookPayload
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64*1024)).Decode(&p); err != nil {
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

func activityEvent(sessionID, cwd, model, nbEvent string) {
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
		"pane_id":      os.Getenv("ZELLIJ_PANE_ID"),
		"session_name": os.Getenv("ZELLIJ_SESSION_NAME"),
	}
	publishEnvelope(chanAgentActivity, env, can)
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
		"pane_id":      os.Getenv("ZELLIJ_PANE_ID"),
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
	_ = Publish(channel, SourceTag(sourceProject), string(body))
}

func newCodexHookSessionStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "session-start",
		Short: "Handle Codex SessionStart hook",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _ := readPayload()
			if p.SessionID == "" {
				return nil
			}
			activityEvent(p.SessionID, p.CWD, p.Model, "started")
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
			p, _ := readPayload()
			if p.SessionID == "" {
				return nil
			}
			activityEvent(p.SessionID, p.CWD, "", "ended")
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
			p, _ := readPayload()
			if p.SessionID != "" {
				activityEvent(p.SessionID, p.CWD, "", "heartbeat")
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
			p, _ := readPayload()
			if p.SessionID != "" {
				activityEvent(p.SessionID, p.CWD, "", "heartbeat")
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
			p, _ := readPayload()
			if p.SessionID == "" {
				return nil
			}
			can, wt := project.Resolve(p.CWD)
			paneID := os.Getenv("ZELLIJ_PANE_ID")
			if paneID == "" {
				paneID = os.Getenv("TMUX_PANE")
			}
			corrID := newUUID()

			toolSummary := p.ToolName
			promptText := ""
			if len(p.Input) > 0 {
				var inputMap map[string]any
				if err := json.Unmarshal(p.Input, &inputMap); err == nil {
					if desc, _ := inputMap["description"].(string); desc != "" {
						toolSummary = p.ToolName + ": " + truncate(desc, 120)
					} else if command, _ := inputMap["command"].(string); command != "" {
						toolSummary = p.ToolName + ": " + truncate(command, 120)
					}
				}
				if b, err := json.Marshal(p.Input); err == nil {
					if len(b) > 200 {
						promptText = string(b[:200])
					} else {
						promptText = string(b)
					}
				}
			}

			now := time.Now().UTC()
			expires := now.Add(5 * time.Minute)
			env := map[string]any{
				"correlation_id": corrID,
				"session_id":     p.SessionID,
				"pane_id":        paneID,
				"project":        can,
				"project_short":  filepath.Base(can),
				"worktree":       wt,
				"tool_name":      p.ToolName,
				"tool_summary":   toolSummary,
				"prompt_text":    promptText,
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
				"deep_link":      "session://" + p.SessionID,
				"ts":             now.Format(time.RFC3339Nano),
				"expires_at":     expires.Format(time.RFC3339Nano),
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