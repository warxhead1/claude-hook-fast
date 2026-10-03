// subagent_join.go — joins Claude Code subagent hook events to their dispatch
// (bus.work.assignment.v1) and emits exactly one terminal `ended` activity
// event per dispatched subagent.
//
// Join key: the Agent tool_use_id that spawned the subagent. It is what
// buildAssignment mints as assignment_id and worker.id. Subagent hook payloads
// carry agent_id (the subagent hex) but not reliably parent_tool_use_id, so the
// key is resolved: payload field, then the subagent's meta.json toolUseId, then
// the registry written when the parent's Agent PostToolUse returned agentId.
//
// Terminal signals (SubagentStop repeats mid-run and cannot be terminal):
//   - the parent's Agent PostToolUse whose tool_response status is not
//     async_launched (a foreground dispatch returned);
//   - a <task-notification> reaching the parent: seen directly on
//     UserPromptSubmit, or in the parent transcript tail at the next
//     PostToolUse/Stop (notifications delivered mid-turn fire no hook).
//
// Each dispatch emits `ended` at most once, guarded by an O_EXCL marker.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// transcriptScanBytes bounds the parent-transcript tail read per hook.
const transcriptScanBytes = 256 * 1024

func joinRegistryDir() string {
	return filepath.Join(os.Getenv("HOME"), ".hearth-loom", "subagent-join")
}

func joinRegPath(prefix, id string) string {
	if !safeID.MatchString(id) {
		return ""
	}
	return filepath.Join(joinRegistryDir(), prefix+"-"+id)
}

func joinRegWrite(prefix, id, content string) {
	p := joinRegPath(prefix, id)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(content), 0o644)
}

func joinRegRead(prefix, id string) string {
	p := joinRegPath(prefix, id)
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func joinRegExists(prefix, id string) bool {
	p := joinRegPath(prefix, id)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// joinRegClaim atomically claims prefix-id; true only for the first caller.
func joinRegClaim(prefix, id string) bool {
	p := joinRegPath(prefix, id)
	if p == "" {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// subagentMetaToolUseID reads toolUseId from the subagent's meta.json, which
// lives beside its transcript under <parent transcript without .jsonl>/subagents/.
func subagentMetaToolUseID(in event.HookInput, hex string) string {
	if hex == "" || !safeID.MatchString(hex) || in.TranscriptPath == "" {
		return ""
	}
	name := "agent-" + hex + ".meta.json"
	var cands []string
	if strings.Contains(in.TranscriptPath, "/subagents/") {
		cands = append(cands, filepath.Join(filepath.Dir(in.TranscriptPath), name))
	} else {
		base := strings.TrimSuffix(in.TranscriptPath, ".jsonl") + "/subagents"
		cands = append(cands, filepath.Join(base, name))
		if m, _ := filepath.Glob(filepath.Join(base, "workflows", "*", name)); len(m) > 0 {
			cands = append(cands, m...)
		}
	}
	for _, c := range cands {
		b, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		var m struct {
			ToolUseID string `json:"toolUseId"`
		}
		if json.Unmarshal(b, &m) == nil && safeID.MatchString(m.ToolUseID) {
			return m.ToolUseID
		}
	}
	return ""
}

// resolveDispatchID returns the Agent tool_use_id that spawned the subagent
// this hook fired in, or "" when no source can establish it.
func resolveDispatchID(in event.HookInput) string {
	if in.ParentToolUseID != "" {
		return in.ParentToolUseID
	}
	hex := in.SubagentHex()
	if hex == "" {
		return ""
	}
	if id := subagentMetaToolUseID(in, hex); id != "" {
		return id
	}
	return joinRegRead("ag", hex)
}

// stampSubagentJoin adds the join fields to a subagent-scoped envelope.
func stampSubagentJoin(env map[string]any, in event.HookInput) {
	if hex := in.SubagentHex(); hex != "" {
		if _, ok := env["subagent_id"]; !ok {
			env["subagent_id"] = hex
		}
	}
	if in.AgentType != "" {
		env["subagent_type"] = in.AgentType
	}
	if id := resolveDispatchID(in); id != "" {
		env["parent_tool_use_id"] = id
		env["assignment_id"] = id
	}
}

var (
	agentIDText = regexp.MustCompile(`agentId:\s*([A-Za-z0-9_-]+)`)
	notifBlock  = regexp.MustCompile(`(?s)<task-notification>(.*?)</task-notification>`)
	tagTUID     = regexp.MustCompile(`<tool-use-id>\s*([A-Za-z0-9_-]+)\s*</tool-use-id>`)
	tagTaskID   = regexp.MustCompile(`<task-id>\s*([A-Za-z0-9_-]+)\s*</task-id>`)
	tagStatus   = regexp.MustCompile(`<status>\s*([A-Za-z_-]+)\s*</status>`)
	tagSummary  = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
)

// endedStatus normalises a completion status to what hearth's registry reads:
// failure-like words contain fail/error/timeout/cancel/panic.
func endedStatus(raw string) string {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "completed", "complete", "done", "success", "succeeded", "ok", "":
		return "completed"
	case "failed", "error", "errored":
		return "failed"
	case "killed", "stopped", "cancelled", "canceled", "aborted":
		return "cancelled"
	default:
		return s
	}
}

// emitSubagentEnded publishes the single terminal event for a dispatched
// subagent. It is published as the subagent worker (host_subagent keyed by the
// dispatch id), not as the parent session that observed the completion.
func emitSubagentEnded(in event.HookInput, toolUseID, hex, status string) bool {
	if !joinRegClaim("ended", toolUseID) {
		return false
	}
	env, can := baseEnvelope(in, "ended", "host_subagent")
	for _, k := range []string{"worktree", "display", "cwd", "transcript_path", "pane_id_qualified", "parent_tool_use_id", "assignment_id", "subagent_id", "subagent_type"} {
		delete(env, k)
	}
	env["parent_tool_use_id"] = toolUseID
	env["assignment_id"] = toolUseID
	env["status"] = endedStatus(status)
	if hex != "" {
		env["subagent_id"] = hex
	}
	publishNbusEnvelope(chanAgentActivity, env, can)
	return true
}

// recordDispatch notes that toolUseID is a subagent dispatch (PreToolUse).
func recordDispatch(toolUseID string) {
	joinRegWrite("tu", toolUseID, time.Now().UTC().Format(time.RFC3339))
}

// handleAgentToolReturn runs on the parent's PostToolUse for Agent/Task.
func handleAgentToolReturn(in event.HookInput) {
	if (in.ToolName != "Agent" && in.ToolName != "Task") || !safeID.MatchString(in.ToolUseID) {
		return
	}
	var resp struct {
		Status  string `json:"status"`
		IsAsync bool   `json:"isAsync"`
		AgentID string `json:"agentId"`
	}
	_ = json.Unmarshal(in.ToolResponse, &resp)
	if resp.AgentID == "" {
		if m := agentIDText.FindSubmatch(in.ToolResponse); m != nil {
			resp.AgentID = string(m[1])
		}
	}
	if resp.AgentID != "" {
		joinRegWrite("ag", resp.AgentID, in.ToolUseID)
	}
	if resp.IsAsync || resp.Status == "async_launched" {
		return
	}
	status := resp.Status
	if in.ToolIsError {
		status = "failed"
	}
	if emitSubagentEnded(in, in.ToolUseID, resp.AgentID, status) {
		hooklog.Writef("nbus-subagent-ended: sync return tool_use_id=%s status=%s", in.ToolUseID, status)
	}
}

// cmdNbusTaskNotification is the UserPromptSubmit hook: a background subagent
// finishing arrives in the parent as a <task-notification> prompt.
func cmdNbusTaskNotification() {
	in, _ := readStdinForHook()
	handleTaskNotifications(in, in.Prompt)
}

// handleTaskNotifications emits `ended` for each notification block in text
// that names a known subagent dispatch.
func handleTaskNotifications(in event.HookInput, text string) {
	if !strings.Contains(text, "<task-notification>") {
		return
	}
	for _, m := range notifBlock.FindAllStringSubmatch(text, -1) {
		blk := m[1]
		tu, st := tagTUID.FindStringSubmatch(blk), tagStatus.FindStringSubmatch(blk)
		if tu == nil || st == nil {
			continue
		}
		if !joinRegExists("tu", tu[1]) {
			sm := tagSummary.FindStringSubmatch(blk)
			if sm == nil || !strings.HasPrefix(strings.TrimSpace(sm[1]), "Agent ") {
				continue
			}
		}
		hex := ""
		if t := tagTaskID.FindStringSubmatch(blk); t != nil {
			hex = t[1]
		}
		if emitSubagentEnded(in, tu[1], hex, st[1]) {
			hooklog.Writef("nbus-subagent-ended: notification tool_use_id=%s status=%s", tu[1], st[1])
		}
	}
}

// scanTranscriptNotifications reads the tail of the parent transcript and
// handles task-notifications delivered to the model without a
// UserPromptSubmit (queued mid-turn). Only user messages and queued_command
// attachments are considered; assistant text quoting the tag is ignored.
func scanTranscriptNotifications(in event.HookInput) {
	if in.TranscriptPath == "" || strings.Contains(in.TranscriptPath, "/subagents/") {
		return
	}
	f, err := os.Open(in.TranscriptPath)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	off := st.Size() - transcriptScanBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return
	}
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if !bytes.Contains(line, []byte("task-notification")) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Attachment struct {
				Type   string `json:"type"`
				Prompt string `json:"prompt"`
			} `json:"attachment"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		var text string
		switch {
		case rec.Type == "user":
			_ = json.Unmarshal(rec.Message.Content, &text)
		case rec.Type == "attachment" && rec.Attachment.Type == "queued_command":
			text = rec.Attachment.Prompt
		}
		handleTaskNotifications(in, text)
	}
}
