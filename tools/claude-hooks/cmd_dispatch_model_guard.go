package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

// cmdDispatchModelGuard blocks Agent/Task dispatches that omit `model`, which
// causes the subagent to silently inherit the session model (Opus).
// Also catches Workflow scripts where some agent() calls lack model:.
//
// Fail-open: any parse error or unrelated tool → exit 0.
// Block signal: exit 2 + human-readable message on stderr.
func cmdDispatchModelGuard() {
	input, ok := hookio.ReadHookInput()
	if !ok {
		return // fail-open: bad stdin
	}

	switch input.ToolName {
	case "Agent", "Task":
		guardAgentModel(input)
	case "Workflow":
		guardWorkflowModels(input)
	default:
		return // unrelated tool — pass
	}
}

func guardAgentModel(input *hookio.HookInput) {
	if len(input.ToolInput) == 0 {
		return // fail-open
	}
	var ti map[string]any
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
		return // fail-open: malformed input
	}

	model := strings.TrimSpace(func() string {
		v, _ := ti["model"].(string)
		return v
	}())

	if model != "" {
		return // explicit model present — pass
	}

	description, _ := ti["description"].(string)
	msg := fmt.Sprintf(`DISPATCH BLOCKED — blank model will inherit session Opus.

  Description: %q

  A blank or missing model causes the subagent to inherit the CURRENT SESSION
  model (Opus). This burns tokens fast — add an explicit model parameter:

    model: "sonnet"   (minimum for any dispatched agent)
    model: "haiku"    (mechanical tasks: file reads, mappings, transforms)
    model: "opus"     (only for the lead session — synthesis / judgment)

  Fix: add model: "sonnet" (or "haiku") to the Agent/Task call.`, description)

	fmt.Fprintln(os.Stderr, msg)
	hooklog.Writef("dispatch-model-guard: BLOCKED Agent/Task (blank model) description=%q", description)
	hookExit(2)
}

func guardWorkflowModels(input *hookio.HookInput) {
	if len(input.ToolInput) == 0 {
		return // fail-open
	}
	var ti map[string]any
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
		return // fail-open
	}

	script, _ := ti["script"].(string)
	if script == "" {
		return // fail-open: no script field
	}

	agentCount := strings.Count(script, "agent(")
	modelCount := strings.Count(script, "model:")

	if agentCount == 0 || modelCount >= agentCount {
		return // fully covered — pass
	}

	msg := fmt.Sprintf(`WORKFLOW BLOCKED — not all agent() calls have model: specified.

  Found %d agent() call(s) but only %d model: spec(s).
  Agents without model: inherit the session Opus model and burn tokens.

  Fix: add  model: 'sonnet'  to every agent() call.
  Example:
    agent("Do the thing", { model: 'sonnet', prompt: "..." })`, agentCount, modelCount)

	fmt.Fprintln(os.Stderr, msg)
	hooklog.Writef("dispatch-model-guard: BLOCKED Workflow (agent=%d model=%d)", agentCount, modelCount)
	hookExit(2)
}
