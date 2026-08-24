package main

import (
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

func newCodexHookLifecycleCmd(use, event string, lifecycle bool) *cobra.Command {
	return &cobra.Command{Use: use, Short: "Handle Codex hook", RunE: func(_ *cobra.Command, _ []string) error {
		p, err := readPayload()
		if err != nil {
			hooklog.WriteEvent("codex_hook_malformed", map[string]any{"hook": use, "error": err.Error()})
			return nil
		}
		emitHook(p, event, lifecycle)
		if p != nil && p.SessionID != "" && event != "ended" {
			ensureLease(p)
		}
		if p != nil && event == "ended" && !isSubagent(p) {
			cleanupLease(p)
			releasePaneOwner(p)
		}
		return nil
	}}
}
func newCodexHookToolCmd(use, event string) *cobra.Command {
	return newCodexHookLifecycleCmd(use, event, false)
}

func newCodexHookPermissionRequestCmd() *cobra.Command {
	return &cobra.Command{Use: "permission-request", Short: "Publish a redacted Codex permission request", RunE: func(_ *cobra.Command, _ []string) error {
		p, err := readPayload()
		if err != nil {
			hooklog.WriteEvent("codex_hook_malformed", map[string]any{"hook": "permission-request", "error": err.Error()})
			return nil
		}
		if p == nil || p.SessionID == "" {
			return nil
		}
		emitHook(p, "permission_requested", false)
		can, wt := project.Resolve(p.CWD)
		raw, binding := qualifiedPane()
		details := toolInputDetailsFor(p)
		now := time.Now().UTC()
		env := map[string]any{"correlation_id": permissionCorrelationID(p), "session_id": p.SessionID, "root_session_id": p.SessionID, "project": can, "project_short": filepath.Base(can), "worktree": wt, "tool_name": p.ToolName, "tool_summary": p.ToolName + ": " + details.Primary, "prompt_text": details.PromptText, "command_preview": details.CommandPreview, "description": details.Description, "justification": details.Justification, "pane_id": raw, "pane_binding": binding, "agent_id": p.AgentID, "agent_type": p.AgentType, "ts": now.Format(time.RFC3339Nano), "expires_at": now.Add(5 * time.Minute).Format(time.RFC3339Nano), "options": []map[string]any{{"label": "Yes", "is_default": true, "checked": false}, {"label": "Yes, and don't ask again for this session", "is_default": false, "checked": false}, {"label": "No", "is_default": false, "checked": false}}}
		if isSubagent(p) || !allowPaneEmission(p, "permission_requested") {
			delete(env, "pane_id")
			delete(env, "pane_binding")
		}
		publishEnvelope("bus.hearth.session.permission.requested.v1", env, can)
		ensureLease(p)
		return nil
	}}
}
func permissionCorrelationID(p *hookPayload) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(stableEventID(p, "permission_requested"))).String()
}
