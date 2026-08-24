// Codex hook integration for hearth-loom. Event parsing, lifecycle emission,
// and lease maintenance live in focused files to keep the hook hot path small.
package main

import (
	"github.com/spf13/cobra"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
)

const (
	chanAgentActivity = "bus.agent.activity.v1"
	chanAgentSession  = "agent.session.v1"
)

func newCodexHookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "codex-hook", Short: "Act as a Codex hook script"}
	cmd.AddCommand(
		newCodexHookLifecycleCmd("session-start", "started", true),
		newCodexHookLifecycleCmd("stop", "turn_end", true),
		newCodexHookLifecycleCmd("session-end", "ended", true),
		newCodexHookLifecycleCmd("user-prompt-submit", "user_prompt", false),
		newCodexHookPreToolCmd(),
		newCodexHookToolCmd("post-tool", "tool_post"),
		newCodexHookToolCmd("pulse", "tool_post"), // legacy PostToolUse configuration
		newCodexHookPermissionRequestCmd(),
		newCodexHookLifecycleCmd("pre-compact", "compact_pre", true),
		newCodexHookLifecycleCmd("post-compact", "compact_post", true),
		newCodexHookLifecycleCmd("subagent-start", "subagent_started", true),
		newCodexHookLifecycleCmd("subagent-stop", "subagent_stopped", true),
		newCodexHookLeaseWorkerCmd(),
		newCodexHookPublishWorkerCmd(),
	)
	return cmd
}

func newCodexHookPublishWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use: nbuspub.WorkerSubcommand + " <channel> <source> <payload>", Hidden: true,
		Args: cobra.ExactArgs(3), RunE: func(_ *cobra.Command, args []string) error {
			nbuspub.WorkerMain(args)
			return nil
		},
	}
}
