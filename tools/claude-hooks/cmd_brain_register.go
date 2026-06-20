// cmd_brain_register.go — Go port of session-start/brain-registration.js
// + lib/brain-client.js (the subset needed for SessionStart).
//
// On SessionStart: registers the agent with the brain service (if available),
// starts a brain session, saves state to /tmp/brain-hook-state-<sid>.json,
// and queries for recent memories (best-effort). Always prints the SessionStart
// ack to stdout regardless of brain availability.
//
// FAF discipline: never panic, never block, never exit nonzero.
// All network and file operations are bounded and errors are swallowed.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------- package-level vars (tests override) ----------

// brainLogPath is the log file for brain hook events.
var brainLogPath = "/tmp/brain-hook.log"

// brainStateDir is the directory where brain hook state files are written.
// State file name: brain-hook-state-<sessionId>.json
var brainStateDir = "/tmp"

// brainBaseURL is the brain service base URL.
var brainBaseURL = "http://127.0.0.1:9100/brain"

// brainHTTPClient is shared with a 2-second timeout (never blocks).
var brainHTTPClient = &http.Client{Timeout: 2 * time.Second}

// ---------- conv ID extraction (DIFFERENT from extractConversationUUID) ----------

// brainConvRE matches "conversations/<uuid>/" in a transcript path.
var brainConvRE = regexp.MustCompile(`conversations/([a-f0-9-]+)/`)

// brainExtractConvID extracts the conversation ID from a transcript path using
// the brain-client.js logic: regex first, then split-on-"conversations" fallback.
// Returns "" when neither path yields a result.
func brainExtractConvID(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}

	// Pattern: .../conversations/<uuid>/...
	if m := brainConvRE.FindStringSubmatch(transcriptPath); m != nil {
		return m[1]
	}

	// Fallback: find "conversations" component and take the next element.
	parts := strings.Split(transcriptPath, "/")
	for i, p := range parts {
		if p == "conversations" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1]
		}
	}

	return ""
}

// ---------- logging ----------

// brainLog appends a timestamped line to brainLogPath. Best-effort.
func brainLog(msg string) {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line := ts + " - " + msg + "\n"
	f, err := os.OpenFile(brainLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// ---------- health check ----------

// brainAvailable returns true if GET /brain/health returns 200. 2s timeout.
func brainAvailable() bool {
	resp, err := brainHTTPClient.Get(brainBaseURL + "/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---------- tool-call POST ----------

// brainRequest POSTs to /brain/tools/call with {"name":"brain","arguments":{action,...args}}.
// Swallows errors (FAF). Returns parsed response map, or nil on failure.
func brainRequest(action string, args map[string]any) map[string]any {
	// Merge action into args.
	payload := make(map[string]any, len(args)+1)
	for k, v := range args {
		payload[k] = v
	}
	payload["action"] = action

	body, err := json.Marshal(map[string]any{
		"name":      "brain",
		"arguments": payload,
	})
	if err != nil {
		brainLog(action + " marshal error: " + err.Error())
		return nil
	}

	resp, err := brainHTTPClient.Post(
		brainBaseURL+"/tools/call",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		brainLog(action + " error: " + err.Error())
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	brainLog(fmt.Sprintf("%s response: %d", action, resp.StatusCode))

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return result
}

// ---------- state management ----------

// brainStateFile returns the path to the state file for a given session.
// When sessionOrDefault is empty it uses "default" (matching JS behaviour).
func brainStateFile(sessionOrDefault string) string {
	key := sessionOrDefault
	if key == "" {
		key = "default"
	}
	return filepath.Join(brainStateDir, "brain-hook-state-"+key+".json")
}

// brainUpdateState merges updates into the persisted state for sessionId.
// Creates the file if it doesn't exist. Best-effort.
func brainUpdateState(sessionId string, updates map[string]any) {
	path := brainStateFile(sessionId)

	// Load existing state (ignore error — fresh state is fine).
	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	// Merge updates.
	for k, v := range updates {
		existing[k] = v
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, out, 0644)
}

// ---------- entry point ----------

// cmdBrainRegister is the Go port of brain-registration.js.
func cmdBrainRegister() {
	// Always emit the SessionStart ack at the end, even on early return.
	defer func() {
		// {"hookSpecificOutput":{"hookEventName":"SessionStart"}}
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName": "SessionStart",
			},
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(os.Stdout, string(b))
	}()

	in, _ := readStdinForHook()
	sessionId := in.SessionID
	transcriptPath := in.TranscriptPath
	source := in.Source
	cwd := in.EffectiveCWD()

	convId := brainExtractConvID(transcriptPath)

	brainLog(fmt.Sprintf("SessionStart: session=%s, conversation=%s, source=%s",
		sessionId, convId, source))

	if !brainAvailable() {
		brainLog("Brain service not available, skipping registration")
		return
	}

	// agentId: "claude-code-<convId>" or "claude-code-<sessionId>" as fallback.
	agentId := "claude-code-"
	if convId != "" {
		agentId += convId
	} else {
		agentId += sessionId
	}

	// Register agent.
	brainRequest("register_agent", map[string]any{
		"agent_id":   agentId,
		"agent_type": "claude_code",
		"capabilities": []string{
			"shader_development",
			"rust_development",
			"c_development",
			"refactoring",
			"testing",
		},
	})
	brainLog("Registered agent: " + agentId)

	// Start session.
	brainRequest("start_session", map[string]any{
		"agent_id": agentId,
	})

	// Build and persist base state.
	// conversationId must be JSON null when convId is empty (*string trick).
	var convIdPtr *string
	if convId != "" {
		convIdPtr = &convId
	}

	baseState := map[string]any{
		"agentId":          agentId,
		"conversationId":   convIdPtr,
		"sessionStartTime": time.Now().UnixMilli(),
		"cwd":              cwd,
		"toolsUsed":        []string{},
		"filesModified":    []string{},
	}
	brainUpdateState(sessionId, baseState)

	// Query for relevant memories (best-effort).
	result := brainRequest("query", map[string]any{
		"memory_type": nil,
		"file_path":   nil,
		"limit":       10,
	})
	if result != nil {
		if memories, ok := result["memories"].([]any); ok && len(memories) > 0 {
			ids := make([]string, 0, len(memories))
			for _, m := range memories {
				if mm, ok := m.(map[string]any); ok {
					if id, ok := mm["id"].(string); ok {
						ids = append(ids, id)
					}
				}
			}
			brainUpdateState(sessionId, map[string]any{
				"loadedContext":  fmt.Sprintf("Loaded %d memories", len(memories)),
				"loadedMemories": ids,
			})
			brainLog(fmt.Sprintf("Loaded %d memories as context", len(memories)))
		}
	}
}
