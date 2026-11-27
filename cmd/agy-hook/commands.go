package main

import (
	"encoding/json"
	"io"
)

// maxHookPayloadBytes bounds stdin reads the same way codex-hook's
// payload.go does — a hook must never let a runaway payload stall the
// agent turn on I/O.
const maxHookPayloadBytes = 1024 * 1024

func readStdinPayload(r io.Reader) orderedMap {
	body, err := io.ReadAll(io.LimitReader(r, maxHookPayloadBytes+1))
	if err != nil || len(body) > maxHookPayloadBytes {
		return orderedMap{}
	}
	return decodePayload(body)
}

// runSessionStart handles Antigravity SessionStart: publishes "started"
// to both channels and writes the Hearth session-binding file (turn="idle")
// so Hearth can address this agy terminal from the start of the session.
func runSessionStart(r io.Reader) {
	payload := readStdinPayload(r)
	env, proj := buildEnvelope(payload, "started", "", "")
	publishEnvelope(chanAgentActivity, env, proj)
	publishEnvelope(chanAgentSession, env, proj)
	writeSessionBinding(payload, "idle")
}

// runPreTool handles Antigravity PreToolUse: publishes question prompts
// for ask_question and subagent_started for invoke_subagent/run_workflow.
func runPreTool(r io.Reader) {
	payload := readStdinPayload(r)
	name, _ := toolInfo(payload)
	if name == "" {
		return
	}
	cwd := effectiveCWD(payload)
	proj, _ := repoSplit(cwd)
	switch name {
	case "ask_question":
		handlePreToolQuestion(payload, cwd, proj)
	case "invoke_subagent", "run_workflow":
		handleSubagentLifecycle(payload, "started", cwd, proj)
	}
}

// runPostTool: PostToolUse is a completed tool call — count it as
// tool_call when a tool name is present, else heartbeat (nervous-bus#34).
// Enriches with tool_use_id, tool_is_error/tool_error_type, and resources,
// and emits question.responded / subagent_stopped when applicable.
func runPostTool(r io.Reader) {
	payload := readStdinPayload(r)
	name, summary := toolInfo(payload)
	event := "heartbeat"
	if name != "" {
		event = "tool_call"
	}
	env, proj := buildEnvelope(payload, event, name, summary)
	if id := toolCallID(payload); id != "" {
		env["tool_use_id"] = id
	}
	if errv, ok := payload.Get("error"); ok && pythonTruthy(errv) {
		env["tool_is_error"] = true
		env["tool_error_type"] = "error"
	}
	if res := extractAgyResources(name, effectiveCWD(payload), toolCallArgs(payload)); len(res) > 0 {
		env["resources"] = res
	}
	publishEnvelope(chanAgentActivity, env, proj)

	cwd := effectiveCWD(payload)
	switch name {
	case "ask_question":
		handlePostToolQuestion(payload, proj)
	case "invoke_subagent", "run_workflow":
		handleSubagentLifecycle(payload, "stopped", cwd, proj)
	}
}

// runPreInvocation refreshes the Hearth session-binding to turn="busy"
// and publishes "started" to both channels on the first invocation of a
// turn (invocationNum <= 1); later invocations within the same turn do
// not re-emit "started".
func runPreInvocation(r io.Reader) {
	payload := readStdinPayload(r)
	writeSessionBinding(payload, "busy")
	if invocationNum(payload) > 1 {
		return
	}
	env, proj := buildEnvelope(payload, "started", "", "")
	publishEnvelope(chanAgentActivity, env, proj)
	publishEnvelope(chanAgentSession, env, proj)
}

// runStop publishes "ended" to bus.agent.activity.v1 (preserving
// reflex-recorder run-close tracking) and "turn_end" to agent.session.v1
// (matching Claude Code's nbus-stop and Hearth's handle_agent_session_stopped),
// marks the Hearth session-binding idle, and harvests any "**Needs you:**"
// block into agent.attention.v1 + ~/.local/state/needs-you/sessions/.
func runStop(r io.Reader) {
	payload := readStdinPayload(r)
	actEnv, proj := buildEnvelope(payload, "ended", "", "")
	sessEnv, _ := buildEnvelope(payload, "turn_end", "", "")
	if tr, ok := payload.Get("terminationReason"); ok && pythonTruthy(tr) {
		actEnv["termination_reason"] = tr
		sessEnv["termination_reason"] = tr
	}
	if errv, ok := payload.Get("error"); ok && pythonTruthy(errv) {
		errStr := truncateRunes(pyStr(errv), 200)
		actEnv["error"] = errStr
		sessEnv["error"] = errStr
	}
	publishEnvelope(chanAgentActivity, actEnv, proj)
	publishEnvelope(chanAgentSession, sessEnv, proj)
	writeSessionBinding(payload, "idle")
	harvestNeedsYou(payload, effectiveCWD(payload), proj)
}

func invocationNum(payload orderedMap) float64 {
	v, ok := payload.Get("invocationNum")
	if !ok {
		return 0
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	return f
}
