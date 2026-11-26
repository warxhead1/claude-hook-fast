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
// causes the subagent to silently inherit the session model.
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

func agentBlockMsg(description string) string {
	return fmt.Sprintf(`DISPATCH BLOCKED — blank model will inherit the session model.

  Description: %q

  A blank or missing model makes the subagent inherit the CURRENT SESSION
  model, the most expensive tier. Set model explicitly, routed by what would
  catch a subtly wrong answer (~/.claude/CLAUDE.md § Model Tiering Discipline):

    model: "haiku"    mechanical, fully specified: apply-these-edits, grep sweeps, verify loops
    model: "sonnet"   testable work: handlers, migrations, adapters, exploration (a gate catches mistakes)
    model: "opus"     craft-heavy implementation from a design that already exists
    (default/Fable)   judgment with no mechanical reviewer: write it in the lead session, do not dispatch it

  Fix: add the tier that fits to the Agent/Task call.`, description)
}

func workflowBlockMsg(agentCount, modelCount int) string {
	return fmt.Sprintf(`WORKFLOW BLOCKED — not all agent() calls have model: specified.

  Found %d agent() call(s) but only %d model: spec(s).
  Agents without model: inherit the session model and burn tokens.

  Fix: add a model tier to every agent() call: 'haiku' (mechanical),
  'sonnet' (testable work), 'opus' (craft from an existing design).
  Example:
    agent("Do the thing", { model: 'sonnet', prompt: "..." })`, agentCount, modelCount)
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
	fmt.Fprintln(os.Stderr, agentBlockMsg(description))
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

	fmt.Fprintln(os.Stderr, workflowBlockMsg(agentCount, modelCount))
	hooklog.Writef("dispatch-model-guard: BLOCKED Workflow (agent=%d model=%d)", agentCount, modelCount)
	hookExit(2)
}
