// Package event models the stdin JSON Claude Code (and opencode) write
// to the hooks they invoke.
//
// Both runtimes share the same overall shape with subtle field-name
// divergences (opencode uses sessionID / tool; Claude Code uses
// session_id / tool_name). We parse both into a single HookInput value
// and let callers decide how to use it.
//
// Field reference (Claude Code hook spec):
//
//	session_id          string  — Claude session uuid
//	transcript_path     string  — path to JSONL transcript (durable
//	                              cross-resume conversation id source)
//	cwd                 string  — Claude's working directory at hook fire
//	tool_name           string  — PostToolUse / PreToolUse only
//	tool_input          json    — PostToolUse / PreToolUse only
//	tool_response       json    — PostToolUse only (Claude Code 1.0.84+)
//	parent_tool_use_id  string  — set when fired inside Agent-tool subsession
//	source              string  — SessionStart: startup|resume|clear|compact
//	message             string  — Notification only
//	stop_reason         string  — Stop only (sometimes)
//
// Anything missing or malformed yields a zero value rather than an
// error — host hooks must never block the runtime.
package event

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

// HookInput is the parsed stdin payload, normalized across runtimes.
// Optional fields are zero-valued when absent.
type HookInput struct {
	SessionID       string          `json:"session_id"`
	TranscriptPath  string          `json:"transcript_path"`
	CWD             string          `json:"cwd"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	ToolResponse    json.RawMessage `json:"tool_response"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Source          string          `json:"source"`
	Message         string          `json:"message"`
	StopReason      string          `json:"stop_reason"`

	// MessageDisplay hook fields.
	MessageText string `json:"message_text"`
	IsPartial   bool   `json:"is_partial"`

	// StopFailure hook fields.
	ErrorType    string `json:"error_type"`
	ErrorMessage string `json:"error_message"`

	// SessionEnd hook fields.
	EndReason string `json:"end_reason"`
	TurnCount int    `json:"turn_count"`

	// SubagentStart / SubagentStop fields.
	SubagentType string `json:"subagent_type"`
	SubagentID   string `json:"subagent_id"`

	// PostToolUse enrichment fields.
	// tool_use_id is the stable ID for the individual tool invocation; it
	// matches the toolUseID recorded in the transcript hook-attachment entry.
	// Passed by Claude Code on stdin since ~2.0.
	ToolUseID string `json:"tool_use_id"`
	// tool_is_error is true when the tool returned an error result (tool_response
	// carries an error payload). Passed by Claude Code on stdin since ~2.1.
	ToolIsError bool `json:"tool_is_error"`

	// opencode-shape aliases. Populated only when the canonical field
	// is empty (Parse() handles the merge).
	SessionIDAlt string `json:"sessionID"`
	ToolAlt      string `json:"tool"`
}

// Parse reads stdin JSON into a HookInput, merging runtime-specific
// aliases into the canonical fields. Returns a zero value on parse
// failure — callers should not propagate non-zero exits.
func Parse(raw []byte) HookInput {
	var h HookInput
	if len(raw) == 0 {
		return h
	}
	_ = json.Unmarshal(raw, &h)

	if h.SessionID == "" && h.SessionIDAlt != "" {
		h.SessionID = h.SessionIDAlt
	}
	if h.ToolName == "" && h.ToolAlt != "" {
		h.ToolName = h.ToolAlt
	}
	return h
}

// ConversationID derives a stable cross-resume handle from the
// transcript path. Returns "" when no transcript was supplied.
//
// Claude Code transcript paths look like
// /home/eric/.claude/projects/<slug>/<conv-id>.jsonl — the file's
// basename without .jsonl is the conversation id and is stable across
// /clear, /compact, and session resume.
func (h HookInput) ConversationID() string {
	if h.TranscriptPath == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(h.TranscriptPath), ".jsonl")
}

// Project returns the canonical project name (e.g. "hearth-loom" not
// "main") for the effective cwd. Delegates to internal/hooks/project
// so the worktree-layout convention is honored uniformly.
//
// Use Worktree() to retrieve the worktree segment ("main", "wt/loom-x")
// separately, or DisplayProject() for the composite "hearth-loom [main]"
// rendering the operator prefers in dashboards.
func (h HookInput) Project() string {
	can, _ := project.Resolve(h.EffectiveCWD())
	return can
}

// Worktree returns the worktree segment for the effective cwd, or ""
// when the path is a direct checkout. See project.Resolve.
func (h HookInput) Worktree() string {
	_, wt := project.Resolve(h.EffectiveCWD())
	return wt
}

// DisplayProject is the "project [worktree]" composite the operator
// wants surfaced in bus events and dashboards.
func (h HookInput) DisplayProject() string {
	can, wt := project.Resolve(h.EffectiveCWD())
	return project.Display(can, wt)
}

// EffectiveCWD prefers hook payload .cwd, falling back to
// $CLAUDE_PROJECT_DIR, then process cwd.
func (h HookInput) EffectiveCWD() string {
	if h.CWD != "" {
		return h.CWD
	}
	if v := os.Getenv("CLAUDE_PROJECT_DIR"); v != "" {
		return v
	}
	if v, err := os.Getwd(); err == nil {
		return v
	}
	return ""
}

// IsSubagent reports whether this hook fired inside a Task-tool subagent
// rather than a top-level Claude Code session.
//
// parent_tool_use_id is the documented signal, but Claude Code does NOT
// populate it on every subagent hook — notably Stop / SubagentStop, where it
// is frequently absent. Trusting it alone let subagents masquerade as
// top-level sessions, which produced two live symptoms:
//   - phantom "Agent stopped — awaiting input" phone notifications for
//     worktree subagents (e.g. "tengine [agent-a65396b02f1705f90]"), and
//   - subagents claiming their parent's inherited pane in the session
//     arbiter, making the frozen-but-live parent vanish from the list.
//
// Three reliable secondary signals cover the cases parent_tool_use_id misses:
//
//  1. Transcript path under a /subagents/ segment
//     (.../subagents/agent-<hex>.jsonl, or
//     .../subagents/workflows/wf_<id>/agent-<hex>.jsonl). A top-level
//     session's transcript is .../<session-uuid>.jsonl and never does.
//     Mirrors steeringTargetID's detection in cmd_nbus.go.
//
//  2. Effective cwd under the legacy agent-dispatch worktree layout,
//     <project>/.claude/worktrees/agent-<hex> (or the .bare/.claude/...
//     variant). This is the one that caught the original phantom: a
//     SubagentStop for a worktree-isolated dispatch carries the PARENT's
//     session_id AND the PARENT's top-level transcript (so signal 1 misses
//     it), but its cwd is the worktree Claude Code minted for the dispatch.
//     That "/.claude/worktrees/agent-" prefix was only ever created for
//     dispatched agents, never a human checkout, so it was definitive.
//     Retained for pre-2026-08-24 sessions / transcripts, but the 2026-08-24
//     relocation ruling moved every NEW agent/isolation worktree off this
//     layout (see signal 3), so this alone stopped catching current
//     dispatches — the exact same phantom-notification failure mode
//     recurred until signal 3 was added.
//
//  3. Effective cwd under the current (2026-08-24+) data2 worktree
//     convention, /home/eric/data2/worktrees/<project>/<name>, via
//     project.IsData2Worktree. <name> may be a dispatch id (agent-<hex>)
//     or a human-chosen isolated-lane name (board-tab, lifecycle-emission,
//     notify-routing are real examples) — every path under this root is an
//     agent/isolation worktree by construction, so the match is on LAYOUT
//     (root prefix + at least two path segments), never an "agent-"
//     substring. Without this, every current-convention dispatched agent's
//     Stop hook falls through to host_claude_code — indistinguishable from
//     a real interactive session, reintroducing the phantom "Agent
//     stopped — awaiting input" notification this function exists to
//     suppress.
func (h HookInput) IsSubagent() bool {
	if h.ParentToolUseID != "" {
		return true
	}
	if strings.Contains(h.TranscriptPath, "/subagents/") {
		return true
	}
	if strings.Contains(h.EffectiveCWD(), "/.claude/worktrees/agent-") {
		return true
	}
	if _, _, ok := project.IsData2Worktree(h.EffectiveCWD()); ok {
		return true
	}
	return false
}

// AgentKind tags whether the hook fired inside a Task-tool subagent
// (host_subagent) or the top-level Claude Code session (host_claude_code).
// Mirrors the bash hook semantics.
func (h HookInput) AgentKind() string {
	if h.IsSubagent() {
		return "host_subagent"
	}
	return "host_claude_code"
}

// PaneIDQualified returns a zellij:<session>:<pane> or tmux:<pane>
// identifier when one is reachable via env. Empty when neither
// multiplexer is present.
func PaneIDQualified() string {
	if s, p := os.Getenv("ZELLIJ_SESSION_NAME"), os.Getenv("ZELLIJ_PANE_ID"); s != "" && p != "" {
		return "zellij:" + s + ":" + p
	}
	if p := os.Getenv("TMUX_PANE"); p != "" {
		return "tmux:" + p
	}
	return ""
}
