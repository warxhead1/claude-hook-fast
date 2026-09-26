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

// runPostTool: PostToolUse is a completed tool call — count it as
// tool_call when a tool name is present, else heartbeat (nervous-bus#34,
// the hot-patch this port must reproduce).
func runPostTool(r io.Reader) {
	payload := readStdinPayload(r)
	name, summary := toolInfo(payload)
	event := "heartbeat"
	if name != "" {
		event = "tool_call"
	}
	env, proj := buildEnvelope(payload, event, name, summary)
	publishEnvelope(chanAgentActivity, env, proj)
}

// runPreInvocation publishes "started" to both channels only on the
// first invocation of a turn (invocationNum <= 1); later invocations
// within the same turn are a no-op, matching the Python script exactly.
func runPreInvocation(r io.Reader) {
	payload := readStdinPayload(r)
	if invocationNum(payload) > 1 {
		return
	}
	env, proj := buildEnvelope(payload, "started", "", "")
	publishEnvelope(chanAgentActivity, env, proj)
	publishEnvelope(chanAgentSession, env, proj)
}

// runStop publishes "ended" to both channels, folding in an optional
// termination reason (passed through verbatim) and/or a truncated,
// stringified error.
func runStop(r io.Reader) {
	payload := readStdinPayload(r)
	env, proj := buildEnvelope(payload, "ended", "", "")
	if tr, ok := payload.Get("terminationReason"); ok && pythonTruthy(tr) {
		env["termination_reason"] = tr
	}
	if errv, ok := payload.Get("error"); ok && pythonTruthy(errv) {
		env["error"] = truncateRunes(pyStr(errv), 200)
	}
	publishEnvelope(chanAgentActivity, env, proj)
	publishEnvelope(chanAgentSession, env, proj)
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
