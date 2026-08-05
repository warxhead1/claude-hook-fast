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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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
// `with_entries(select(.value != "" and .value != null))`) and hands
// off to a detached publish worker — the hook never waits on nervous.
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
	_ = nbuspub.PublishAsync(channel, nbuspub.SourceTag(sourceProject), string(body))
}

// cmdNbusActivity replaces zzy-nbus-activity.sh — PostToolUse → bus.agent.activity.v1.
// Skips Grep/Glob to keep the signal lean.
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
	switch in.ToolName {
	case "Grep", "Glob":
		return
	}

	env, can := baseEnvelope(in, "tool_call", in.AgentKind())
	env["tool_name"] = in.ToolName
	env["tool_summary"] = boundToolSummary(in.ToolName, in.ToolInput)
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

	// AskUserQuestion answered: PostToolUse fires for it (confirmed empirically)
	// and the result carries the user's selection in tool_response.answers as
	// {question: chosen_label}. Emit a grounded question.responded.v1 keyed by
	// the same tool_use_id the ask used, so hearth dismisses the exact in-app
	// card with the real choice — no activity heuristics, no transcript polling.
	if in.ToolName == "AskUserQuestion" {
		var answers []string
		qaMap := map[string]string{}
		var m map[string]any
		if err := json.Unmarshal(in.ToolResponse, &m); err == nil {
			if a, ok := m["answers"].(map[string]any); ok {
				for q, v := range a {
					if s, ok := v.(string); ok && s != "" {
						answers = append(answers, s)
						qaMap[q] = s
					}
				}
			}
		}
		renv := map[string]any{
			"correlation_id":      in.ToolUseID,
			"session_id":          in.SessionID,
			"answers":             answers,
			"answers_by_question": qaMap,
			"answered_by":         "terminal",
			"ts":                  time.Now().UTC().Format(time.RFC3339Nano),
		}
		publishNbusEnvelope(chanSessionQuestionResponded, renv, can)
	}
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

		// Write the real context file so pane_to_jsonl_index() can find this
		// session even when running in headless/print mode (-p flag), where
		// the statusLine hook never fires and the pending-* file never gets
		// upgraded to a real context file.
		if tmuxPane := os.Getenv("TMUX_PANE"); tmuxPane != "" {
			xdg := os.Getenv("XDG_RUNTIME_DIR")
			if xdg == "" {
				xdg = fmt.Sprintf("/run/user/%d", os.Getuid())
			}
			ctxDir := filepath.Join(xdg, "hearth", "context")
			_ = os.MkdirAll(ctxDir, 0755)
			ctxPayload, _ := json.Marshal(map[string]any{
				"currentContext": 0,
				"tmux_pane":      tmuxPane,
				"zellij_pane_id": nil,
				"pid":            os.Getpid(),
			})
			ctxPath := filepath.Join(ctxDir, "claude-context-"+in.SessionID+".json")
			if err := os.WriteFile(ctxPath, ctxPayload, 0644); err == nil {
				hooklog.Writef("nbus-session-start: wrote context file for %s pane=%s", in.SessionID, tmuxPane)
			}
		}
	}
	emitSessionLifecycleFromInput(in, "started")
}

func cmdNbusStop() {
	in, _ := readStdinForHook()
	emitSessionLifecycleFromInput(in, "turn_end")
	maybeHarvestOnStop(in)
}

// harvestCursorPath returns the path to the cursor file tracking the last
// Stop-triggered kb harvest for a session. Separate directory from
// steeringCursorPath (~/.hearth-loom/steering-cursor/) — different
// semantics (wall-clock gate vs a "since" pull cursor) — so the two
// features can't collide by keying on the same session id.
func harvestCursorPath(sessionID string) string {
	home := os.Getenv("HOME")
	return filepath.Join(home, ".hearth-loom", "harvest-cursor", sessionID)
}

// harvestStopMinInterval bounds how often a Stop event may trigger a kb
// harvest for the same session. kb harvest today only fires from
// cmdNbusPreCompact — during a long uncompacted stretch (observed: a real
// session ran 5,726 JSONL lines / 3 days with zero compaction) nothing gets
// harvested until the next compaction, or never if the session ends first.
// Stop fires on every turn end, so gating it at this interval closes that
// gap without harvesting on every single turn.
const harvestStopMinInterval = 15 * time.Minute

// maybeHarvestOnStop fires a detached, best-effort `kb harvest` for the
// session's transcript when enough wall-clock time has passed since the
// last Stop-triggered harvest for this session. Detached (Start, not
// Run/Wait) so this never adds latency to the Stop hook chain
// (tmux-ready → brain-session-summary.js → nbus-stop → cleanup-tmp) — the
// harvest child process outlives this hook invocation.
func maybeHarvestOnStop(in event.HookInput) {
	if in.SessionID == "" || in.TranscriptPath == "" {
		return
	}

	cursorPath := harvestCursorPath(in.SessionID)
	if data, err := os.ReadFile(cursorPath); err == nil {
		var lastNano int64
		if json.Unmarshal(data, &lastNano) == nil && lastNano > 0 {
			if time.Since(time.Unix(0, lastNano)) < harvestStopMinInterval {
				return
			}
		}
	}

	spec := resolveKbHarvest(in)
	if spec.Binary == "" {
		return
	}
	// Stop-triggered harvest is a pure vault side-effect — no --stdout
	// injection (that's PreCompact's job, where the digest is meant to be
	// read back into context).
	spec.Args = stripFlag(spec.Args, "--stdout")

	if !startDetached(spec.Binary, spec.Args) {
		return
	}

	// Advance the cursor optimistically; kb harvest is idempotent
	// (content_hash dedup) so a spurious "already harvested" on the very
	// next Stop is harmless — worst case we wait one more interval.
	now := time.Now().UnixNano()
	_ = os.MkdirAll(filepath.Dir(cursorPath), 0755)
	if b, err := json.Marshal(now); err == nil {
		_ = os.WriteFile(cursorPath, b, 0644)
	}
}

// stripFlag removes a bare (non-valued) flag from an args slice.
func stripFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			continue
		}
		out = append(out, a)
	}
	return out
}

// startDetached launches bin with args as a detached child (new session,
// via Setsid) and returns immediately without waiting, so the child
// survives this hook process exiting. Best-effort: any Start() error is
// swallowed after logging.
func startDetached(bin string, args []string) bool {
	cmd := osexec.Command(bin, args...) //nolint:gosec
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = append(os.Environ(), "NERVOUS_BUS_LIVE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		hooklog.Writef("nbus-stop: kb harvest start error: %v", err)
		return false
	}
	return true
}
func cmdNbusNotification() { emitSessionLifecycle("notification") }

// kbHarvestSpec holds the resolved binary path and argument slice for a kb
// harvest invocation. An empty Binary means "no harvest" (skip silently).
type kbHarvestSpec struct {
	Binary string
	Args   []string
}

// resolveKbHarvest returns the spec for harvesting a compaction transcript
// into the kb knowledge vault. Returns zero spec (empty Binary) when the
// kb binary cannot be found or TranscriptPath is empty — callers skip
// silently in that case.
//
// Binary resolution order:
//  1. $HOME/.local/bin/kb
//  2. kb on PATH
func resolveKbHarvest(in event.HookInput) kbHarvestSpec {
	if in.TranscriptPath == "" {
		return kbHarvestSpec{}
	}

	// Prefer the canonical install location.
	home := os.Getenv("HOME")
	if home != "" {
		candidate := filepath.Join(home, ".local", "bin", "kb")
		if _, err := os.Stat(candidate); err == nil {
			return kbHarvestSpec{
				Binary: candidate,
				Args: []string{
					"harvest",
					"--transcript", in.TranscriptPath,
					"--session", in.SessionID,
					"--stdout",
					"--max-tokens", "2000",
				},
			}
		}
	}

	// Fall back to PATH lookup.
	if bin, err := osexec.LookPath("kb"); err == nil { //nolint:gosec
		return kbHarvestSpec{
			Binary: bin,
			Args: []string{
				"harvest",
				"--transcript", in.TranscriptPath,
				"--session", in.SessionID,
				"--stdout",
				"--max-tokens", "2000",
			},
		}
	}

	return kbHarvestSpec{}
}

// runKbHarvest executes the kb harvest command described by spec, captures
// its stdout, and writes it to out (the hook's own stdout — injection
// mechanism). The execution is bounded by an 8-second context timeout.
// All errors are swallowed per FAF discipline; on failure nothing is
// written to out to avoid injecting partial garbage. Returns the captured
// digest bytes (nil on failure/empty) so callers can also inspect it, e.g.
// for the auto-landmark decision-phrase scan.
func runKbHarvest(spec kbHarvestSpec, out io.Writer) []byte {
	if spec.Binary == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	cmd := osexec.CommandContext(ctx, spec.Binary, spec.Args...) //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	// kb's bus publisher is gated behind NERVOUS_BUS_LIVE (off by default,
	// to keep `cargo test` runs quiet) — hook-triggered harvests are real
	// session activity, not test noise, so opt them in explicitly.
	cmd.Env = append(os.Environ(), "NERVOUS_BUS_LIVE=1")

	if err := cmd.Run(); err != nil {
		hooklog.Writef("nbus-pre-compact: kb harvest error: %v", err)
		return nil
	}

	if buf.Len() == 0 {
		return nil
	}
	if _, err := io.Copy(out, &buf); err != nil {
		hooklog.Writef("nbus-pre-compact: kb harvest stdout copy error: %v", err)
	}
	return buf.Bytes()
}

// decisionPhraseRe matches sentence-starting decision-shaped language —
// the same shape a CLAUDE.md hardline convention uses ("we should X",
// "never Y", "always Z", "decided to Q"). Anchored at line start (harvest
// digest lines are one ask/outcome/commit per line, see assemble_body in
// kb's harvest.rs) to avoid matching mid-sentence noise like "...has never
// been tested".
var decisionPhraseRe = regexp.MustCompile(`(?im)^(?:[-*>]\s*)?(?:we (?:should|must|will) |never |always |decided to |decision: )([^\n.]{10,200})`)

// maxAutoLandmarksPerCompact bounds the auto-landmark blast radius per
// PreCompact event — this is a cheap unvetted capture, not a substitute
// for `kb save-finding`'s human/agent-judged pillar classification.
const maxAutoLandmarksPerCompact = 3

// autoLandmarkFromDigest scans a kb-harvest digest for decision-shaped
// sentences and calls `kb landmark` (default confidence 0.3, tagged
// auto-landmark) for each — the automated half of session-time
// idea/brainstorm curation. `kb save-finding` stays manual: pillar
// classification needs judgment a regex can't make. Detached + capped +
// deduped; every error swallowed (FAF discipline, matches runKbHarvest).
func autoLandmarkFromDigest(spec kbHarvestSpec, in event.HookInput, digest []byte) {
	if len(digest) == 0 || spec.Binary == "" {
		return
	}
	matches := decisionPhraseRe.FindAllStringSubmatch(string(digest), -1)
	if len(matches) == 0 {
		return
	}

	can, _ := project.Resolve(in.EffectiveCWD())
	seen := make(map[string]bool, len(matches))
	count := 0
	for _, m := range matches {
		if count >= maxAutoLandmarksPerCompact {
			break
		}
		title := strings.TrimSpace(m[1])
		if title == "" || seen[title] {
			continue
		}
		seen[title] = true
		count++

		args := []string{"landmark", title, "--project", can, "--tags", "auto-landmark,precompact"}
		if !startDetached(spec.Binary, args) {
			return
		}
	}
}

func cmdNbusPreCompact() {
	// Read stdin exactly once so both the bus emit and the harvest can use it.
	in, _ := readStdinForHook()
	emitSessionLifecycleFromInput(in, "compact")

	// Resolve and run kb harvest. The spec is a pure function of in so it
	// can be tested without spawning the real binary. runKbHarvest writes
	// the digest to os.Stdout — injected into the post-compact context —
	// and returns it so the auto-landmark scan can inspect the same text.
	spec := resolveKbHarvest(in)
	if spec.Binary == "" {
		if in.TranscriptPath != "" {
			hooklog.Writef("nbus-pre-compact: kb binary not found, skipping harvest")
		}
		return
	}
	digest := runKbHarvest(spec, os.Stdout)
	autoLandmarkFromDigest(spec, in, digest)
}

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
	// R6/X1: emit the bare pane_id the hearth pane arbiter keys on so
	// turn_end/started become a HookReported pane-binding edge — the
	// strongest arbiter hint (design §2.2). Deliberately NOT context_pct
	// (R6: obsoleted by hearth's server-side token computation).
	//
	if pane := sessionLifecyclePaneID(in); pane != "" {
		env["pane_id"] = pane
	}
	publishNbusEnvelope(chanAgentSession, env, can)
}

// sessionLifecyclePaneID returns the pane_id to bind on a lifecycle envelope,
// or "" to omit it.
//
// Subagent-inheritance guard: a Task-tool subagent hook fires with
// ParentToolUseID set (AgentKind() == "host_subagent") but inherits the
// PARENT's TMUX_PANE/ZELLIJ_PANE_ID env. Emitting pane_id for it would bind
// the subagent's session_id to the parent's pane and corrupt hearth's
// one-pane-one-session invariant, so we suppress it and bind only for a
// top-level session. (transcript_path already rides baseEnvelope for both.)
func sessionLifecyclePaneID(in event.HookInput) string {
	if in.IsSubagent() {
		return ""
	}
	return paneIDForArbiter()
}

// paneIDForArbiter returns the bare pane identifier the hearth pane arbiter
// keys on: raw tmux "%N" (hearth stores tmux panes unprefixed, as
// discover_agent_panes emits them), or the qualified "zellij:<session>:<pane>"
// form. Empty when neither multiplexer is present. This differs from
// event.PaneIDQualified only in that tmux is left unprefixed (no "tmux:").
func paneIDForArbiter() string {
	if s, p := os.Getenv("ZELLIJ_SESSION_NAME"), os.Getenv("ZELLIJ_PANE_ID"); s != "" && p != "" {
		return "zellij:" + s + ":" + p
	}
	return os.Getenv("TMUX_PANE")
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
	chanPermissionRequested      = "bus.hearth.session.permission.requested.v1"
	chanSessionQuestion          = "bus.hearth.session.question.v1"
	chanSessionQuestionResponded = "bus.hearth.session.question.responded.v1"
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

	now := time.Now().UTC()
	projShort := filepath.Base(can)

	// AskUserQuestion carries a real question + answer options from the model.
	// Publish to the dedicated question channel instead of the generic permission
	// card, so phone/watch can render the real choices.
	//
	// This handler is called from BOTH PreToolUse (AskUserQuestion matcher) and
	// PermissionRequest (catch-all). PreToolUse always fires first and provides
	// tool_use_id. PermissionRequest fires afterward with no tool_use_id. To avoid
	// triple notifications, skip the PermissionRequest path when tool_use_id is set
	// (PreToolUse already published).
	if in.ToolName == "AskUserQuestion" {
		// Dedup: if we have no tool_use_id, we're the PermissionRequest invocation —
		// PreToolUse already handled this. Skip.
		if in.ToolUseID == "" {
			hooklog.Writef("nbus-permission-request: AskUserQuestion skip (PermissionRequest dup; PreToolUse already published) session=%s", in.SessionID)
			return
		}

		type parsedQ struct {
			text    string
			options []string
		}
		var allQuestions []parsedQ

		if len(in.ToolInput) > 0 {
			var m map[string]any
			if err := json.Unmarshal(in.ToolInput, &m); err == nil {
				// Primary shape: {"questions": [{"question": "...", "options": [...]}]}
				if qs, ok := m["questions"].([]any); ok && len(qs) > 0 {
					for _, qi := range qs {
						q, ok := qi.(map[string]any)
						if !ok {
							continue
						}
						text, _ := q["question"].(string)
						var opts []string
						if rawOpts, ok := q["options"].([]any); ok {
							for _, o := range rawOpts {
								switch v := o.(type) {
								case string:
									opts = append(opts, v)
								case map[string]any:
									if label, ok := v["label"].(string); ok && label != "" {
										opts = append(opts, label)
									}
								}
							}
						}
						if text != "" {
							allQuestions = append(allQuestions, parsedQ{text: text, options: opts})
						}
					}
				}
				// Fallback: top-level "question" field
				if len(allQuestions) == 0 {
					if text, _ := m["question"].(string); text != "" {
						allQuestions = append(allQuestions, parsedQ{text: text})
					}
				}
			}
		}
		if len(allQuestions) == 0 {
			allQuestions = append(allQuestions, parsedQ{text: "AskUserQuestion"})
		}

		// Correlate via tool_use_id so PostToolUse can resolve the exact card.
		qCorr := in.ToolUseID

		// Build the questions_all array for multi-phase survey support.
		// Android renders a SurveyCard when questions_all has >1 entry, so all
		// phases are visible upfront instead of being lost when Q1 is answered.
		questionsAll := make([]map[string]any, 0, len(allQuestions))
		for _, q := range allQuestions {
			questionsAll = append(questionsAll, map[string]any{
				"question": q.text,
				"options":  q.options,
			})
		}

		env := map[string]any{
			"correlation_id": qCorr,
			"session_id":     in.SessionID,
			"question":       allQuestions[0].text,
			"options":        allQuestions[0].options,
			"questions_all":  questionsAll,
			"project":        can,
			"project_short":  projShort,
			"ts":             now.Format(time.RFC3339Nano),
		}
		publishNbusEnvelope(chanSessionQuestion, env, can)
		hooklog.Writef("nbus-permission-request: published QUESTION corr=%s session=%s pane=%s phases=%d",
			qCorr, in.SessionID, paneID, len(allQuestions))
		return
	}

	// All other tools: publish the generic permission/approval card.
	// Extract exactly what the tool is doing so the watch shows a useful card,
	// not just "Claude Code wants permission". Each tool type has its own field.
	toolSummary, promptText := extractToolAction(in.ToolName, in.ToolInput)

	expires := now.Add(5 * time.Minute)

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
		// Schema bus.hearth.session.permission.requested.v1 requires options as
		// structured objects {label, is_default, checked} — emitting bare strings
		// makes redis-mirror dead-letter the event ("'No' is not of type 'object'")
		// so it never reaches hearth-api and no card is ever shown.
		"options": []map[string]any{
			{"label": "Yes", "is_default": true, "checked": false},
			{"label": "Yes, and don't ask again for this session", "is_default": false, "checked": false},
			{"label": "No", "is_default": false, "checked": false},
		},
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
	in, raw := readStdinForHook()
	// Temporary (2026-08-05): real MessageDisplay events parse to
	// msg_chars=0, so the payload schema differs from our struct tags.
	// Capture the raw head until the true field names are confirmed.
	traceNote("raw_head", truncate(string(raw), 300))
	// Evidence trail for the message-drop investigation (2026-08-05): record
	// every message the client delivers to the hook layer, including the
	// ones we skip, so a vanished assistant message can be located — traced
	// here means the client had it at display time; absent means it was
	// dropped before any hook ran.
	traceNote("session", in.SessionID)
	traceNote("is_partial", in.IsPartial)
	traceNote("msg_chars", len(in.MessageText))
	traceNote("msg_preview", truncate(in.MessageText, 200))
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

	// Registry hygiene (see cmd_session_registry.go). Skipped for
	// reason=="clear": the process continues running under a freshly
	// rotated session_id, so its registry file isn't actually ending —
	// CC (or session-init's stale-sessionId repair, for older CC builds)
	// handles that rewrite on the next SessionStart instead.
	if in.EndReason == "clear" {
		return
	}
	srTombstoneEndedSession(srSessionsDir(), in.SessionID, in.EndReason, os.Getpid(), time.Now())
	srResetPaneTitle()
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

// largeInputTools carry semantically-rich, multi-KB tool_input (a dispatch
// prompt) — for these we keep a bigger tool_summary so the dispatched directive
// is visible on the bus, not clipped to its opening line. Ordinary tool inputs
// (Bash/Read/Edit) are compact JSON that fits in the default bound, so a global
// raise would just waste bytes. Empirically (dispatch-observability-audit): only
// ~0.7% of activity events are dispatches; raising just these 200->1000 costs
// ~0.08% of debug.jsonl growth while lifting prompt-byte capture 5%->25%.
var largeInputTools = map[string]bool{"Agent": true, "Task": true, "Workflow": true}

const (
	toolSummaryBound      = 200  // default: compact tool inputs
	toolSummaryBoundLarge = 1000 // dispatch prompts (largeInputTools)
)

// boundToolSummary picks the bound from the tool name, then summarizes.
func boundToolSummary(toolName string, raw json.RawMessage) string {
	n := toolSummaryBound
	if largeInputTools[toolName] {
		n = toolSummaryBoundLarge
	}
	return boundedToolSummaryN(raw, n)
}

// boundedToolSummary returns the first 200 chars of the compacted tool_input.
// Retained for callers that don't vary by tool; equivalent to the default bound.
func boundedToolSummary(raw json.RawMessage) string {
	return boundedToolSummaryN(raw, toolSummaryBound)
}

// boundedToolSummaryN returns the first n chars of the compacted tool_input.
// Empty when no input was supplied.
func boundedToolSummaryN(raw json.RawMessage, n int) string {
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
	if len(compact) > n {
		return string(compact[:n])
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
