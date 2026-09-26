package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/codexpolicy"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/shellguard"
)

type preToolDecision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func newCodexHookPreToolCmd() *cobra.Command {
	return &cobra.Command{Use: "pre-tool", Short: "Publish and enforce Codex pre-tool policy", RunE: func(_ *cobra.Command, _ []string) error {
		p, err := readPayload()
		if err != nil {
			hooklog.WriteEvent("codex_hook_malformed", map[string]any{"hook": "pre-tool", "error": err.Error()})
			return nil
		}
		emitHook(p, "tool_pre", false)

		// Destructive-shell guards (rm -r outside scratch, sudo, bare git
		// stash/pop, git reset --hard to a remote ref, git clean -f) run
		// FIRST and unconditionally — a bad worker-policy load must never
		// mask them, and they apply regardless of which model launched the
		// command. See internal/hooks/shellguard for the single-source
		// mirror of the Claude-side ~/.claude/hook-rules.json guards.
		for _, command := range shellCommands(p) {
			if os.Getenv("CODEX_SHELL_GUARD_DISABLE") == "1" {
				break // kill switch, mirrors RM_GUARD_DISABLE on the Claude side
			}
			if v := shellguard.Inspect(command, p.CWD); v != nil {
				hooklog.WriteEvent("codex_shell_guard_blocked", map[string]any{"rule_id": v.RuleID})
				return writePreToolBlock(v.Message)
			}
		}

		policy, err := codexpolicy.Load()
		if err != nil {
			reason := fmt.Sprintf("Codex worker launch policy is invalid and has failed closed: %v", err)
			hooklog.WriteEvent("codex_worker_policy_invalid", map[string]any{"error": err.Error()})
			return writePreToolBlock(reason)
		}
		for _, command := range shellCommands(p) {
			if violations := codexpolicy.InspectCommand(command, policy); len(violations) > 0 {
				hooklog.WriteEvent("codex_worker_launch_blocked", map[string]any{"reason": violations[0].Reason, "model": violations[0].Model})
				return writePreToolBlock(violations[0].Reason)
			}
		}
		return nil
	}}
}

func shellCommands(p *hookPayload) []string {
	if p == nil || len(p.effectiveInput()) == 0 {
		return nil
	}
	var input map[string]any
	if json.Unmarshal(p.effectiveInput(), &input) != nil {
		return nil
	}
	var commands []string
	for _, key := range []string{"command", "cmd"} {
		switch value := input[key].(type) {
		case string:
			commands = append(commands, value)
		case []any:
			for _, item := range value {
				if command, ok := item.(string); ok {
					commands = append(commands, command)
				}
			}
		}
	}
	return commands
}

func writePreToolBlock(reason string) error {
	var decision preToolDecision
	decision.HookSpecificOutput.HookEventName = "PreToolUse"
	decision.HookSpecificOutput.PermissionDecision = "deny"
	decision.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(os.Stdout).Encode(decision)
}
