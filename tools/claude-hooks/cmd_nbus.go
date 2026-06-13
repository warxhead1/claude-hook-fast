// cmd_nbus.go — Go replacements for the bash nbus-publish scaffolding
// under ~/.claude/hooks/*/nbus-publish.sh and zzy-nbus-activity.sh.
//
// Subcommands:
//
//	nbus-activity       — global PostToolUse → bus.agent.activity.v1
//	                       (replaces post-tool-use/zzy-nbus-activity.sh)
//	nbus-session-start  — SessionStart → agent.session (event=started)
//	                       (replaces session-start/nbus-publish.sh)
//	nbus-stop           — Stop turn-end → agent.session (event=turn_end)
//	nbus-notification   — Notification → agent.session (event=notification)
//	nbus-pre-compact    — PreCompact → agent.session (event=compact)
//	register-with-hearth-loom — SessionStart CCM register
//	                       (replaces session-start/register-with-hearth-loom.sh)
//	deregister          — Stop / SubagentStop → daemon hook endpoint
//	                       (replaces the staged-but-undeployed
//	                        zzy-deregister-subagent-stop.sh)
//	steering-poll       — PreToolUse one-shot pull of pending steering
//	                       commands addressed to this session. SCAFFOLDING
//	                       only — the receive-side endpoint lands in a
//	                       follow-up bead; for now the call is best-effort
//	                       and logs "no commands" when the endpoint 404s.
//
// All emissions stamp `project: <canonical>` and `worktree: <wt>` plus
// a `display: "<canonical> [<wt>]"` field, per the operator's
// rendering preference. ResolveProject (internal/hooks/project) handles
// the worktree → canonical mapping.
//
// FAF discipline: every subcommand swallows errors and exits 0. Logs
// land in ~/.claude/hooks/log/hearth-loom-host-hook.log via hooklog.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/client"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/transcript"
)

// execCmd is a thin alias so the linter's G204 check sees a typed
// wrapper rather than a bare os/exec.Command call.
func execCmd(name string, args ...string) *osexec.Cmd {
	return osexec.Command(name, args...) //nolint:gosec
}

const (
	hearthLoomDefaultURL = "http://127.0.0.1:5908"

	chanAgentActivity = "bus.agent.activity.v1"
	chanAgentSession  = "agent.session.v1"

	stdinCapBytes = 1 << 20
)

// readStdinForHook caps stdin and returns (parsed input, raw bytes).
// Returns (zero HookInput, nil) on read failure — callers should
// silently exit when the result is empty.
func readStdinForHook() (event.HookInput, []byte) {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, stdinCapBytes))
	if err != nil {
		return event.HookInput{}, nil
	}
	return event.Parse(raw), raw
}

// hearthLoomURL returns $HEARTH_LOOM_URL or the default.
func hearthLoomURL() string {
	if v := strings.TrimSpace(os.Getenv("HEARTH_LOOM_URL")); v != "" {
		return v
	}
	return hearthLoomDefaultURL
}

// baseEnvelope builds the project/worktree/display fields every nbus
// emission carries. Returns the canonical project for callers that
// also need the NERVOUS_SOURCE override.
func baseEnvelope(in event.HookInput, eventName, agentKind string) (map[string]any, string) {
	can, wt := project.Resolve(in.EffectiveCWD())
	env := map[string]any{
		"ts":                 time.Now().UTC().Format(time.RFC3339Nano),
		"event":              eventName,
		"agent":              "claude-code",
		"agent_kind":         agentKind,
		"agent_id":           in.SessionID,
		"session_id":         in.SessionID,
		"conversation_id":    in.ConversationID(),
		"project":            can,
		"worktree":           wt,
		"display":            project.Display(can, wt),
		"cwd":                in.EffectiveCWD(),
		"pane_id_qualified":  event.PaneIDQualified(),
		"transcript_path":    in.TranscriptPath,
		"source_kind":        in.Source,
		"parent_tool_use_id": in.ParentToolUseID,
	}
	return env, can
}

// publishNbusEnvelope drops empty values (mirrors the bash
// `with_entries(select(.value != "" and .value != null))`) and shells
// out via nervous publish.
func publishNbusEnvelope(channel string, env map[string]any, sourceProject string) {
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
		hooklog.Writef("nbus marshal %s: %v", channel, err)
		return
	}
	_ = nbuspub.Publish(channel, nbuspub.SourceTag(sourceProject), string(body))
}

// cmdNbusActivity replaces zzy-nbus-activity.sh — PostToolUse → bus.agent.activity.v1.
// Emits all tools (incl. Read/Grep/Glob) so navigation-based detectors get data.
//
// Enrichment (additive, all fields optional so existing consumers are unaffected):
//   - tool_use_id        — stable tool-invocation ID (from hook stdin, Claude Code ~2.0+)
//   - tool_is_error      — bool: true when the tool returned an error result
//   - tool_error_type    — short error label extracted from tool_response when tool_is_error
//   - input_tokens       — API call input token count (from transcript tail)
//   - output_tokens      — API call output token count (from transcript tail)
//   - cache_read_tokens  — cache-read input tokens (from transcript tail)
//   - cache_write_tokens — cache-creation input tokens (from transcript tail)
//   - model              — model id (from transcript tail, e.g. "claude-sonnet-4-6")
//
// Token fields are emitted as integers when non-zero, omitted when zero or
// unavailable (transcript unreadable, stale, pre-Claude Code 1.0.84).
// Cost is intentionally NOT computed here — that belongs downstream.
func cmdNbusActivity() {
	in, _ := readStdinForHook()
	if in.ToolName == "" {
		return
	}
	// Read/Grep/Glob ARE emitted: the Reflexarc file-reads-to-finding and
	// reread-same-file detectors depend on navigation events. (Was: skipped to
	// keep the signal lean; that starved 2 detectors — see nervous-bus jvzkn.3.)

	env, can := baseEnvelope(in, "tool_call", in.AgentKind())
	env["tool_name"] = in.ToolName
	env["tool_summary"] = boundedToolSummary(in.ToolInput)
	env["tool_response_summary"] = boundedToolResponseSummary(in.ToolResponse)

	// --- Enrichment block ---

	// tool_use_id: stable per-invocation ID, present in Claude Code ~2.0+ stdin.
	if in.ToolUseID != "" {
		env["tool_use_id"] = in.ToolUseID
	}

	// tool_is_error + tool_error_type: structured outcome for failure detection.
	if in.ToolIsError {
		env["tool_is_error"] = true
		if errType := extractToolErrorType(in.ToolResponse); errType != "" {
			env["tool_error_type"] = errType
		}
	}

	// Token usage: cheap tail-read of the transcript (last 8 KB only).
	// Falls through silently if the transcript is unavailable or stale.
	if in.TranscriptPath != "" {
		if u := transcript.LatestUsage(in.TranscriptPath); !u.IsZero() {
			env["input_tokens"] = u.InputTokens
			env["output_tokens"] = u.OutputTokens
			if u.CacheReadInputTokens > 0 {
				env["cache_read_tokens"] = u.CacheReadInputTokens
			}
			if u.CacheCreationInputTokens > 0 {
				env["cache_write_tokens"] = u.CacheCreationInputTokens
			}
			if u.Model != "" {
				env["model"] = u.Model
			}
		}
	}

	publishNbusEnvelope(chanAgentActivity, env, can)
}

// extractToolErrorType returns a short label from a tool_response error payload.
// Claude Code wraps tool errors as {"type":"tool_result","is_error":true,"content":"..."}.
// We look for a type-discriminator or fall back to "error". Returns "" when the
// response is not parseable as an error.
func extractToolErrorType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	// Prefer an explicit "type" field if present and non-empty.
	if t, ok := m["type"].(string); ok && t != "" && t != "tool_result" {
		return truncate(t, 64)
	}
	// Fall back to "error" as a generic label.
	return "error"
}

func cmdNbusSessionStart() {
	in, _ := readStdinForHook()
	// Persist session ID to ~/.hearth-loom/active-session-id so hearth-loom
	// CLI commands (loomie spawn/tree) automatically know the orchestrator
	// session without requiring HEARTH_LOOM_SESSION_ID to be set in the env.
	if in.SessionID != "" {
		path := filepath.Join(os.Getenv("HOME"), ".hearth-loom", "active-session-id")
		_ = os.WriteFile(path, []byte(in.SessionID), 0644)
		hooklog.Writef("nbus-session-start: wrote active-session-id=%s", in.SessionID)
	}
	emitSessionLifecycleFromInput(in, "started")
}

func cmdNbusStop() {
	in, _ := readStdinForHook()
	emitSessionLifecycleFromInput(in, "turn_end")
}
func cmdNbusNotification()  { emitSessionLifecycle("notification") }
func cmdNbusPreCompact()    { emitSessionLifecycle("compact") }

// emitSessionLifecycle is the agent.session emitter the bash
// lib/nbus-publish.sh used to do. Single helper for callers that haven't
// already consumed stdin; see emitSessionLifecycleFromInput for callers
// that have.
func emitSessionLifecycle(nbEvent string) {
	in, _ := readStdinForHook()
	emitSessionLifecycleFromInput(in, nbEvent)
}

func emitSessionLifecycleFromInput(in event.HookInput, nbEvent string) {
	if in.SessionID == "" {
		// No session id → nothing to correlate downstream. The bash
		// hook would emit anyway with empty fields; we suppress to
		// keep the bus signal-rich.
		return
	}
	env, can := baseEnvelope(in, nbEvent, in.AgentKind())
	if in.Message != "" {
		env["message"] = in.Message
	}
	publishNbusEnvelope(chanAgentSession, env, can)
}

// cmdRegisterWithHearthLoom replaces session-start/register-with-hearth-loom.sh.
// POSTs the CCM register payload using the new project resolver so the
// daemon ties the session to the canonical (project, worktree) pair.
func cmdRegisterWithHearthLoom() {
	in, _ := readStdinForHook()
	can, wt := project.Resolve(in.EffectiveCWD())

	paneID := os.Getenv("ZELLIJ_PANE_ID")
	if paneID == "" {
		paneID = os.Getenv("TMUX_PANE")
	}
	if paneID == "" {
		paneID = fmt.Sprintf("shell-%d", os.Getpid())
	}

	payload := map[string]any{
		"pane_id":    paneID,
		"cwd":        in.EffectiveCWD(),
		"agent_type": "claude-code",
		"session_id": in.SessionID,
		"project":    can,
		"worktree":   wt,
		"display":    project.Display(can, wt),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		hooklog.Writef("register marshal: %v", err)
		return
	}
	// Reuse the CCM register endpoint convention from the bash script.
	// The daemon listens at $HEARTH_LOOM_URL (default 127.0.0.1:5908)
	// or $HEARTH_LOOM_ADDR (legacy bash env, host:port — accepted for
	// drop-in compatibility).
	url := hearthLoomURL() + "/api/v2/ccm/register"
	if addr := strings.TrimSpace(os.Getenv("HEARTH_LOOM_ADDR")); addr != "" {
		url = "http://" + addr + "/api/v2/ccm/register"
	}
	if err := client.PostJSON(url, body); err != nil {
		hooklog.Writef("register: %v", err)
	}
}

// cmdDeregister forwards Stop / SubagentStop to the daemon's session
// hook endpoint. The event name is derived from the subcommand
// argument: `deregister Stop` or `deregister SubagentStop`. When no
// argument is supplied we default to "Stop" to match the bash hook
// convention (one script per directory).
func cmdDeregister() {
	eventName := "Stop"
	if flag.NArg() >= 2 && flag.Arg(1) != "" {
		eventName = flag.Arg(1)
	}
	in, raw := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	url := fmt.Sprintf("%s/api/v2/sessions/%s/hook/%s",
		hearthLoomURL(), in.SessionID, eventName)
	if err := client.PostJSON(url, raw); err != nil {
		hooklog.Writef("deregister[%s]: %v", eventName, err)
	}
}

// steeringTargetID returns the id this hook invocation should poll/steer
// against, and a bool that is true when the id is a self-identified subagent.
//
// Claude Code writes a per-agent JSONL transcript. A Task-tool subagent's
// transcript lives at
//
//	.../subagents/agent-<hex>.jsonl
//	.../subagents/workflows/wf_<id>/agent-<hex>.jsonl
//
// where <hex> is the subagent's stable agentId (it equals the `agentId`
// field inside the transcript and is distinct per subagent, while the
// shared session_id is the PARENT's). The top-level (parent) transcript is
// .../<session-uuid>.jsonl and does NOT live under /subagents/.
//
// When the transcript path self-identifies a subagent we return the bare
// hex (no "agent-" prefix, no ".jsonl") so a steer POSTed to
// /sessions/<hex>/commands reaches exactly that subagent. Otherwise we
// return the session_id unchanged — today's behavior.
//
// Fail-safe: any path that does not match the subagent shape, or whose hex
// is empty, falls through to (sessionID, false). The detection is a cheap
// string check and never errors.
func steeringTargetID(transcriptPath, sessionID string) (string, bool) {
	if transcriptPath == "" || !strings.Contains(transcriptPath, "/subagents/") {
		return sessionID, false
	}
	base := filepath.Base(transcriptPath)
	if !strings.HasPrefix(base, "agent-") {
		return sessionID, false
	}
	hex := strings.TrimSuffix(strings.TrimPrefix(base, "agent-"), ".jsonl")
	if hex == "" {
		return sessionID, false
	}
	return hex, true
}

// steeringCursorPath returns the path to the cursor file for the given session.
// The cursor stores the last-seen UnixNano timestamp so each poll only receives
// commands created after the previous poll. Stored under ~/.hearth-loom/steering-cursor/.
//
// The id may be a session uuid OR a subagent hex (see steeringTargetID). Both
// are slash-free single path segments, so the join stays inside the cursor dir.
func steeringCursorPath(sessionID string) string {
	home := os.Getenv("HOME")
	return filepath.Join(home, ".hearth-loom", "steering-cursor", sessionID)
}

// readSteeringCursor reads the last-seen Unix-nano timestamp for a session.
// Returns 0 on any read/parse error (first poll returns everything).
func readSteeringCursor(sessionID string) int64 {
	data, err := os.ReadFile(steeringCursorPath(sessionID))
	if err != nil {
		return 0
	}
	var nano int64
	if err := json.Unmarshal(data, &nano); err != nil {
		return 0
	}
	return nano
}

// writeSteeringCursor persists the last-seen Unix-nano timestamp. Best-effort.
func writeSteeringCursor(sessionID string, nano int64) {
	path := steeringCursorPath(sessionID)
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.Marshal(nano)
	_ = os.WriteFile(path, data, 0644)
}

// steeringCommandText extracts a human-readable string from a SteeringCommand
// (deserialized as map[string]any from the GET response JSON).
//
// Priority:
//  1. command — top-level shorthand field (e.g. -d '{"command":"..."}')
//  2. payload.text — structured payload, text key
//  3. payload.message — structured payload, message key
//  4. type — fall-back label when nothing else is set
func steeringCommandText(cmd map[string]any) string {
	// 1. Top-level command shorthand.
	if c, ok := cmd["command"].(string); ok && c != "" {
		return c
	}
	// 2-3. Structured payload.
	if payload, ok := cmd["payload"].(map[string]any); ok {
		if text, ok := payload["text"].(string); ok && text != "" {
			return text
		}
		if msg, ok := payload["message"].(string); ok && msg != "" {
			return msg
		}
	}
	// 4. Type as last-resort label.
	if t, ok := cmd["type"].(string); ok && t != "" {
		return "[type:" + t + "]"
	}
	return "[steering command]"
}

// cmdSteeringPoll polls the daemon for pending steering commands addressed to
// this session and outputs a systemMessage so Claude Code injects them as a
// turn message. Uses a per-session cursor file to avoid re-surfacing commands
// seen in previous turns.
//
// Output (when commands are present):
//
//	{"systemMessage": "⚡ Steering: <text>\n<text2>"}
//
// Best-effort: daemon down or 404 → silent. Timeout=1s enforced by the
// settings.json hook entry; hook must exit quickly to not delay tool use.
func cmdSteeringPoll() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	// Self-identify a subagent from its transcript path so steers can be
	// addressed to the individual subagent rather than the shared parent
	// session. Falls back to session_id on any non-match (fail-safe).
	targetID, isSubagent := steeringTargetID(in.TranscriptPath, in.SessionID)
	since := readSteeringCursor(targetID)
	pollURL := fmt.Sprintf("%s/api/v2/sessions/%s/commands?since=%d",
		hearthLoomURL(), targetID, since)
	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "claude-hook-fast/steering-poll")
	resp, err := client.Shared.Do(req)
	if err != nil {
		// Daemon down → silent. Hooks must not block.
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable {
		// Endpoint not yet live or store not wired — silent to keep the hook lean.
		return
	}
	if resp.StatusCode >= 300 {
		hooklog.Writef("steering-poll: status=%d id=%s", resp.StatusCode, targetID)
		return
	}
	if len(body) == 0 || string(body) == "[]" || string(body) == "[]\n" {
		return
	}

	// Parse the response array of SteeringCommands.
	var commands []map[string]any
	if err := json.Unmarshal(body, &commands); err != nil {
		hooklog.Writef("steering-poll: parse error id=%s: %v", targetID, err)
		return
	}
	if len(commands) == 0 {
		return
	}

	// Advance cursor to the latest CreatedAt so these commands aren't re-surfaced.
	var maxNano int64
	for _, cmd := range commands {
		if ts, ok := cmd["created_at"].(string); ok && ts != "" {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if n := t.UnixNano(); n > maxNano {
					maxNano = n
				}
			}
		}
	}
	if maxNano > since {
		writeSteeringCursor(targetID, maxNano)
	}

	// Format message lines.
	var lines []string
	for _, cmd := range commands {
		lines = append(lines, steeringCommandText(cmd))
	}
	msg := strings.Join(lines, "\n")
	idKind := "session"
	if isSubagent {
		idKind = "subagent"
	}
	hooklog.Writef("steering-poll: injecting %d command(s) %s=%s", len(lines), idKind, targetID)

	out := map[string]any{
		"systemMessage": "⚡ Steering: " + msg,
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}

// cmdLoomieCheckCompletions is called from the Stop hook. It reads
// ~/.hearth-loom/completions/*.json, surfaces any that belong to the
// current active session as a systemMessage (shown in the Claude Code
// UI), calls notify-send for desktop alert, and moves the files to
// completions/processed/ so they aren't reported twice.
func cmdLoomieCheckCompletions() {
	home := os.Getenv("HOME")
	completionsDir := filepath.Join(home, ".hearth-loom", "completions")
	processedDir := filepath.Join(completionsDir, "processed")
	_ = os.MkdirAll(processedDir, 0755)

	// Read the active session ID written by cmdNbusSessionStart.
	sessionIDBytes, err := os.ReadFile(filepath.Join(home, ".hearth-loom", "active-session-id"))
	if err != nil || len(strings.TrimSpace(string(sessionIDBytes))) == 0 {
		return
	}
	activeSession := strings.TrimSpace(string(sessionIDBytes))

	entries, err := os.ReadDir(completionsDir)
	if err != nil {
		return
	}

	var messages []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(completionsDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		spawnedBy, _ := payload["spawned_by_session"].(string)
		if spawnedBy != activeSession {
			continue
		}

		slug, _ := payload["slug"].(string)
		execID, _ := payload["execution_id"].(string)
		status, _ := payload["status"].(string)
		prURL, _ := payload["pr_url"].(string)

		name := slug
		if name == "" && len(execID) >= 8 {
			name = execID[:8]
		} else if name == "" {
			name = execID
		}

		line := fmt.Sprintf("loomie %s: %s", name, status)
		if prURL != "" {
			line += " — " + prURL
		}
		messages = append(messages, line)
		hooklog.Writef("loomie-check-completions: %s", line)

		// Desktop notification — best-effort, non-blocking.
		go func(msg string) {
			//nolint:gosec // notify-send args are not user-controlled
			_ = runQuiet("notify-send", "-t", "8000", "hearth-loom", msg)
		}(line)

		// Archive the file so it won't be reported again.
		_ = os.Rename(path, filepath.Join(processedDir, e.Name()))
	}

	if len(messages) == 0 {
		return
	}

	// Output systemMessage so Claude Code surfaces it in the UI after the turn.
	out := map[string]any{
		"systemMessage": "🤖 loomie completions:\n" + strings.Join(messages, "\n"),
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}

// runQuiet runs a command discarding stdout/stderr. Returns error if
// the binary doesn't exist or exits non-zero — callers should ignore.
func runQuiet(name string, args ...string) error {
	cmd := execCmd(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

const (
	chanPermissionRequested = "bus.hearth.session.permission.requested.v1"
	chanSessionMessage      = "bus.hearth.session.message.v1"
	chanSessionError        = "bus.hearth.session.error.v1"
	chanSessionEnd          = "bus.hearth.session.end.v1"
	chanSubagentLifecycle   = "bus.hearth.subagent.lifecycle.v1"
)

// cmdNbusPermissionRequest handles the PermissionRequest hook — fires when
// Claude Code is about to show a permission prompt to the user. Publishes to
// bus.hearth.session.permission.requested.v1 so the phone/watch shows an
// Approve/Deny card instantly, without polling tmux pane captures.
//
// The hook returns nothing (exit 0), so Claude Code still shows the terminal
// prompt — the user can respond there OR via the watch card. Both paths send
// a keystroke back to the pane through the existing reactor/approve flow.
func cmdNbusPermissionRequest() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	can, wt := project.Resolve(in.EffectiveCWD())
	paneID := event.PaneIDQualified()
	corrID := uuid.New().String()

	// Extract exactly what the tool is doing so the watch shows a useful card,
	// not just "Claude Code wants permission". Each tool type has its own field.
	toolSummary, promptText := extractToolAction(in.ToolName, in.ToolInput)

	now := time.Now().UTC()
	expires := now.Add(5 * time.Minute)
	projShort := filepath.Base(can)

	env := map[string]any{
		"correlation_id": corrID,
		"session_id":     in.SessionID,
		"pane_id":        paneID,
		"project":        can,
		"project_short":  projShort,
		"worktree":       wt,
		"tool_name":      in.ToolName,
		"tool_summary":   toolSummary,
		"prompt_text":    promptText,
		"options":        []string{"Yes", "Yes, and don't ask again for this session", "No"},
		"deep_link":      "session://" + in.SessionID,
		"ts":             now.Format(time.RFC3339Nano),
		"expires_at":     expires.Format(time.RFC3339Nano),
	}
	publishNbusEnvelope(chanPermissionRequested, env, can)
	hooklog.Writef("nbus-permission-request: published corr=%s session=%s tool=%s pane=%s",
		corrID, in.SessionID, in.ToolName, paneID)
}

// cmdNbusMessageDisplay handles MessageDisplay (is_partial=false) — the
// authoritative real-time source for session last output. Replaces JSONL
// polling for last_output on phone/watch.
func cmdNbusMessageDisplay() {
	in, _ := readStdinForHook()
	if in.SessionID == "" || in.IsPartial || in.MessageText == "" {
		return
	}
	can, wt := project.Resolve(in.EffectiveCWD())
	env, _ := baseEnvelope(in, "message_display", in.AgentKind())
	env["message_text"] = truncate(in.MessageText, 2000)
	env["project"] = can
	env["worktree"] = wt
	publishNbusEnvelope(chanSessionMessage, env, can)
}

// cmdNbusStopFailure handles StopFailure — API error at turn end.
func cmdNbusStopFailure() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	can, _ := project.Resolve(in.EffectiveCWD())
	env, _ := baseEnvelope(in, "stop_failure", in.AgentKind())
	env["error_type"] = in.ErrorType
	env["error_message"] = in.ErrorMessage
	publishNbusEnvelope(chanSessionError, env, can)
}

// cmdNbusSessionEnd handles SessionEnd — session terminated (vs Stop = turn end).
func cmdNbusSessionEnd() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	can, _ := project.Resolve(in.EffectiveCWD())
	env, _ := baseEnvelope(in, "session_end", in.AgentKind())
	env["end_reason"] = in.EndReason
	env["turn_count"] = in.TurnCount
	publishNbusEnvelope(chanSessionEnd, env, can)
}

// cmdNbusSubagentLifecycle handles SubagentStart and SubagentStop.
// The lifecycle event (started|stopped) is the first CLI argument.
func cmdNbusSubagentLifecycle() {
	lifecycle := "started"
	if flag.NArg() >= 2 {
		lifecycle = flag.Arg(1)
	}
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	can, _ := project.Resolve(in.EffectiveCWD())
	env, _ := baseEnvelope(in, "subagent_"+lifecycle, in.AgentKind())
	env["subagent_type"] = in.SubagentType
	env["subagent_id"] = in.SubagentID
	publishNbusEnvelope(chanSubagentLifecycle, env, can)
}

// extractToolAction returns (summary, fullDetail) for a permission prompt.
// summary is a short actionable label for the notification title/watch chip.
// fullDetail is the full command/path shown in the notification body and watch card.
func extractToolAction(toolName string, rawInput json.RawMessage) (summary, detail string) {
	if len(rawInput) == 0 {
		return toolName, toolName
	}
	var m map[string]any
	if err := json.Unmarshal(rawInput, &m); err != nil {
		return toolName, toolName
	}

	str := func(key string) string {
		v, _ := m[key].(string)
		return v
	}

	switch toolName {
	case "Bash":
		cmd := str("command")
		if cmd == "" {
			return "Bash", "Bash"
		}
		// First line of command as summary; full command as detail.
		firstLine := cmd
		if nl := strings.IndexByte(cmd, '\n'); nl > 0 {
			firstLine = cmd[:nl]
		}
		summary = "Bash: " + truncate(firstLine, 80)
		detail = truncate(cmd, 560) // schema maxLength=600; truncate to leave room for encoding overhead
		return

	case "Edit", "MultiEdit":
		path := str("file_path")
		if path == "" {
			path = str("path")
		}
		summary = toolName + ": " + filepath.Base(path)
		detail = path
		return

	case "Write":
		path := str("file_path")
		if path == "" {
			path = str("path")
		}
		content := str("content")
		summary = "Write: " + filepath.Base(path)
		detail = path + "\n" + truncate(content, 300)
		return

	case "Read":
		path := str("file_path")
		if path == "" {
			path = str("path")
		}
		summary = "Read: " + filepath.Base(path)
		detail = path
		return

	case "Glob":
		pattern := str("pattern")
		summary = "Glob: " + truncate(pattern, 80)
		detail = truncate(pattern, 560)
		return

	case "Grep":
		pattern := str("pattern")
		searchPath := str("path")
		if searchPath != "" {
			summary = "Grep: " + truncate(pattern, 60) + " in " + filepath.Base(searchPath)
		} else {
			summary = "Grep: " + truncate(pattern, 80)
		}
		detail = truncate("grep "+pattern+" "+searchPath, 560)
		return

	case "WebFetch":
		url := str("url")
		summary = "WebFetch: " + truncate(url, 80)
		detail = truncate(url, 560)
		return

	case "WebSearch":
		query := str("query")
		summary = "WebSearch: " + truncate(query, 80)
		detail = truncate(query, 560)
		return

	case "AskUserQuestion":
		// The question text is what the user actually needs to answer.
		var question string
		if qs, ok := m["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				question, _ = q["question"].(string)
			}
		}
		if question == "" {
			question, _ = m["question"].(string)
		}
		if question == "" {
			return "AskUserQuestion", "AskUserQuestion"
		}
		summary = truncate(question, 80)
		detail = truncate(question, 560)
		return

	case "mcp__":
		// MCP tool — use the full tool name and input as-is.
		fallthrough
	default:
		// Unknown tool: show the full JSON input so the user can make an
		// informed decision instead of seeing "wants permission".
		b, _ := json.Marshal(m)
		detail = truncate(string(b), 400)
		if desc := str("description"); desc != "" {
			summary = toolName + ": " + truncate(desc, 80)
		} else {
			summary = toolName + ": " + truncate(detail, 80)
		}
		return
	}
}

// truncate caps a string at max runes without splitting UTF-8.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// boundedToolSummary returns the first 200 chars of the compacted
// tool_input. Empty when no input was supplied.
func boundedToolSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	compact := []byte(raw)
	if err := json.Unmarshal(raw, &v); err == nil {
		if buf, err := json.Marshal(v); err == nil {
			compact = buf
		}
	}
	if len(compact) > 200 {
		return string(compact[:200])
	}
	return string(compact)
}

// boundedToolResponseSummary returns the first 300 chars of the compacted
// tool_response. Returns "" for nil, empty, JSON null, empty object or empty
// array — callers rely on the empty string to omit the field via
// publishNbusEnvelope's elision loop.
func boundedToolResponseSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", "{}", "[]":
		return ""
	}
	// Compact the JSON to remove unnecessary whitespace.
	var v any
	compact := []byte(raw)
	if err := json.Unmarshal(raw, &v); err == nil {
		if buf, err := json.Marshal(v); err == nil {
			compact = buf
		}
	}
	return truncate(string(compact), 300)
}
