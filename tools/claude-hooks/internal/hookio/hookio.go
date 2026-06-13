package hookio

import (
	"encoding/json"
	"os"
	"strings"
)

type HookInput struct {
	ToolName           string          `json:"tool_name"`
	ToolInput         json.RawMessage `json:"tool_input,omitempty"`
	Parameters        map[string]any  `json:"parameters,omitempty"`
	RequiresApproval  bool            `json:"requires_approval,omitempty"`
	RequiresPermission bool           `json:"requires_permission,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	TranscriptPath    string          `json:"transcript_path,omitempty"`
	ConversationID    string          `json:"conversation_id,omitempty"`
	Message           string          `json:"message,omitempty"`
	PermissionMode    string          `json:"permission_mode,omitempty"`
}

func ReadHookInput() (*HookInput, bool) {
	data, err := os.ReadFile("/dev/stdin")
	if err != nil {
		return nil, false
	}
	if len(data) == 0 {
		return nil, false
	}
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, false
	}
	return &input, true
}

func ExtractConversationID(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	parts := strings.Split(transcriptPath, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasPrefix(parts[i], "conversation-") {
			return strings.TrimSuffix(parts[i], ".json")
		}
	}
	return ""
}
