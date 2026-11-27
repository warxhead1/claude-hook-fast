// Package main implements the agy-hook CLI: subcommands mirroring the
// wiring in ~/.gemini/config/hooks.json (session-start, pre-tool,
// post-tool, pre-invocation, stop), each reading a camelCase JSON payload
// from stdin and publishing nervous-bus envelopes via the shared nbuspub
// package — the same publish path claude-hook-fast and codex-hook use.
//
// Every subcommand exits 0 no matter what happens internally: PostToolUse
// and SessionStart expect {}, PreInvocation treats {} as "no injected
// steps", PreToolUse expects {"decision":"allow"}, and Stop treats {} as
// default termination. This preserves the fail-open contract so telemetry
// never breaks an agy turn.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
)

const (
	chanAgentActivity            = "bus.agent.activity.v1"
	chanAgentSession             = "agent.session.v1"
	chanAgentAttention           = "agent.attention.v1"
	chanSessionQuestion          = "bus.hearth.session.question.v1"
	chanSessionQuestionResponded = "bus.hearth.session.question.responded.v1"
	chanSubagentLifecycle        = "bus.hearth.subagent.lifecycle.v1"
)

// publishAsync is a deterministic test seam, same pattern as codex-hook.
var publishAsync = nbuspub.PublishAsync

func newAgyHookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agy-hook", Short: "Act as an Antigravity (agy) hook script"}
	cmd.AddCommand(
		newAgyHookSessionStartCmd(),
		newAgyHookPreToolCmd(),
		newAgyHookPostToolCmd(),
		newAgyHookPreInvocationCmd(),
		newAgyHookStopCmd(),
		newAgyHookPublishWorkerCmd(),
	)
	return cmd
}

// runFailOpen wraps a hook body so a panic from malformed input can never
// escape (mirroring the Python script's outer `try: ... except: pass`).
func runFailOpen(stdin io.Reader, body func(io.Reader)) error {
	defer func() { _ = recover() }()
	body(stdin)
	return nil
}

func newAgyHookSessionStartCmd() *cobra.Command {
	return &cobra.Command{Use: "session-start", Short: "Handle Antigravity SessionStart", RunE: func(cmd *cobra.Command, _ []string) error {
		err := runFailOpen(os.Stdin, runSessionStart)
		fmt.Fprintln(cmd.OutOrStdout(), "{}")
		return err
	}}
}

func newAgyHookPreToolCmd() *cobra.Command {
	return &cobra.Command{Use: "pre-tool", Short: "Handle Antigravity PreToolUse", RunE: func(cmd *cobra.Command, _ []string) error {
		err := runFailOpen(os.Stdin, runPreTool)
		fmt.Fprintln(cmd.OutOrStdout(), `{"decision":"allow"}`)
		return err
	}}
}

func newAgyHookPostToolCmd() *cobra.Command {
	return &cobra.Command{Use: "post-tool", Short: "Handle Antigravity PostToolUse", RunE: func(cmd *cobra.Command, _ []string) error {
		err := runFailOpen(os.Stdin, runPostTool)
		fmt.Fprintln(cmd.OutOrStdout(), "{}")
		return err
	}}
}

func newAgyHookPreInvocationCmd() *cobra.Command {
	return &cobra.Command{Use: "pre-invocation", Short: "Handle Antigravity PreInvocation", RunE: func(cmd *cobra.Command, _ []string) error {
		err := runFailOpen(os.Stdin, runPreInvocation)
		fmt.Fprintln(cmd.OutOrStdout(), "{}")
		return err
	}}
}

func newAgyHookStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Handle Antigravity Stop", RunE: func(cmd *cobra.Command, _ []string) error {
		err := runFailOpen(os.Stdin, runStop)
		fmt.Fprintln(cmd.OutOrStdout(), "{}")
		return err
	}}
}

func newAgyHookPublishWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use: nbuspub.WorkerSubcommand + " <channel> <source> <payload>", Hidden: true,
		Args: cobra.ExactArgs(3), RunE: func(_ *cobra.Command, args []string) error {
			nbuspub.WorkerMain(args)
			return nil
		},
	}
}

func publishEnvelope(channel string, env map[string]any, sourceProject string) {
	publishEnvelopeWithSource(channel, env, nbuspub.SourceTag(sourceProject))
}

func publishEnvelopeWithSource(channel string, env map[string]any, source string) {
	body, err := jsonMarshal(env)
	if err != nil {
		hooklog.WriteEvent("agy_hook_marshal_failed", map[string]any{"channel": channel, "error": err.Error()})
		return
	}
	if err := publishAsync(channel, source, body); err != nil {
		hooklog.WriteEvent("agy_hook_publish_failed", map[string]any{"channel": channel, "error": err.Error()})
	}
}
