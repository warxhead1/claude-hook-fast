package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxHookPayloadBytes = 1024 * 1024

// hookPayload deliberately models the exact Codex 0.149 fields. Unknown
// fields are ignored; malformed input is fail-open and recorded diagnostically.
type hookPayload struct {
	SessionID            string          `json:"session_id"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	TranscriptPath       string          `json:"transcript_path"`
	AgentTranscriptPath  string          `json:"agent_transcript_path"`
	CWD                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	Model                string          `json:"model"`
	PermissionMode       string          `json:"permission_mode"`
	TurnID               string          `json:"turn_id"`
	Source               string          `json:"source"`
	Reason               string          `json:"reason"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolUseID            string          `json:"tool_use_id"`
	StopHookActive       bool            `json:"stop_hook_active"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Prompt               string          `json:"prompt"`
	Trigger              string          `json:"trigger"`
	Input                json.RawMessage `json:"input"` // pre-0.149 compatibility
}

func readPayload() (*hookPayload, error) { return readPayloadFrom(os.Stdin) }
func readPayloadFrom(r io.Reader) (*hookPayload, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxHookPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHookPayloadBytes {
		return nil, fmt.Errorf("hook payload exceeds %d bytes", maxHookPayloadBytes)
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
func (p *hookPayload) effectiveInput() json.RawMessage {
	if p == nil {
		return nil
	}
	if len(p.ToolInput) > 0 && string(p.ToolInput) != "null" {
		return p.ToolInput
	}
	return p.Input
}

var secretArgPattern = regexp.MustCompile(`(?i)(--?(?:api[-_]?key|token|secret|password|authorization|credential|private[-_]?key)|(?:api[-_]?key|token|secret|password|authorization|credential|private[-_]?key))(\s+|=)(?:"[^"]*"|'[^']*'|[^\s]+)`)

type toolInputDetails struct{ Primary, CommandPreview, Description, Justification, PromptText string }

func toolInputDetailsFor(p *hookPayload) toolInputDetails {
	if p == nil || len(p.effectiveInput()) == 0 {
		return toolInputDetails{}
	}
	var value any
	if json.Unmarshal(p.effectiveInput(), &value) != nil {
		return toolInputDetails{}
	}
	d := toolInputDetails{PromptText: boundedJSON(redactValue(value), 200)}
	if m, ok := value.(map[string]any); ok {
		d.Description, _ = m["description"].(string)
		d.Justification, _ = m["justification"].(string)
		if command, ok := m["command"]; ok {
			d.CommandPreview = boundedValue(command, 200)
			d.Primary = d.CommandPreview
		} else if action, _ := m["action"].(string); action != "" {
			d.Primary = truncate(redactString(action), 200)
		}
	}
	if d.Primary == "" {
		d.Primary = boundedValue(value, 200)
	}
	d.Description = truncate(redactString(d.Description), 200)
	d.Justification = truncate(redactString(d.Justification), 200)
	return d
}
func codexToolSummary(p *hookPayload) string { return toolInputDetailsFor(p).Primary }
func boundedValue(v any, max int) string {
	switch x := v.(type) {
	case string:
		return truncate(redactString(x), max)
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			parts = append(parts, boundedValue(p, max))
		}
		return truncate(strings.Join(parts, " "), max)
	default:
		return boundedJSON(redactValue(x), max)
	}
}
func boundedJSON(v any, max int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return truncate(string(b), max)
}
func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			if isSecretKey(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redactValue(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = redactValue(item)
		}
		return out
	case string:
		return redactString(x)
	default:
		return v
	}
}
func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "token") || strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "credential") || strings.Contains(k, "authorization") || strings.Contains(k, "apikey") || strings.Contains(k, "api_key") || strings.Contains(k, "api-key") || strings.Contains(k, "private_key") || strings.Contains(k, "private-key")
}
func redactString(v string) string { return secretArgPattern.ReplaceAllString(v, "$1$2[REDACTED]") }
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
