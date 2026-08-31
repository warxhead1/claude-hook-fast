package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/tmux"
)

func cmdTmuxPreparing() {
	if os.Getenv("TMUX") == "" {
		fmt.Println(`{}`)
		return
	}

	input, ok := hookio.ReadHookInput()
	if !ok {
		fmt.Println(`{}`)
		return
	}

	windowID, _ := tmux.GetWindowID()

	toolName := input.ToolName
	if toolName == "" {
		toolName = "unknown"
	}

	statusText := tmux.FormatToolStatus(toolName)
	wid := windowID
	if wid == "" {
		wid = "current"
	}
	_ = tmux.UpdateTmuxWindow(statusText, wid, "tmux-preparing")

	appendHookLog(fmt.Sprintf("%s - tmux-preparing: %s\n", timestamp(), statusText))
	fmt.Println(`{}`)
}

func cmdPlanTracker() {
	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}

	if input.ToolName != "ExitPlanMode" {
		return
	}

	conversationID := hookio.ExtractConversationID(input.TranscriptPath)
	if conversationID == "" {
		return
	}

	_ = conversationID
	appendHookLog(fmt.Sprintf("%s - plan-tracker: captured ExitPlanMode\n", timestamp()))
}

func cmdAgentHeartbeat() {
	if os.Getenv("TMUX") == "" {
		return
	}

	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}

	conversationID := hookio.ExtractConversationID(input.TranscriptPath)
	if conversationID == "" {
		return
	}

	skipTools := map[string]bool{"Grep": true, "Glob": true}
	if skipTools[input.ToolName] {
		return
	}

	_ = conversationID
	appendHookLog(fmt.Sprintf("%s - agent-heartbeat: %s\n", timestamp(), input.ToolName))
}

func cmdFeedbackCollector() {
	input, ok := hookio.ReadHookInput()
	if !ok {
		fmt.Println(`{"continue": true}`)
		return
	}

	toolName := input.ToolName
	toolInput := input.ToolInput

	shaderExt := map[string]bool{
		".slang": true, ".glsl": true, ".hlsl": true, ".wgsl": true,
		".vert": true, ".frag": true, ".comp": true, ".geom": true,
		".tesc": true, ".tese": true, ".task": true, ".mesh": true,
		".rgen": true, ".rint": true, ".rahit": true, ".rchit": true,
		".rmiss": true, ".rcall": true,
	}

	isShaderEdit := false
	if toolInput != nil {
		var ti map[string]any
		if err := json.Unmarshal(toolInput, &ti); err == nil {
			if fp, ok := ti["file_path"].(string); ok {
				ext := filepath.Ext(fp)
				if shaderExt[ext] && containsAny(toolName, "Write", "Edit", "MultiEdit") {
					isShaderEdit = true
				}
			}
		}
	}

	if !isShaderEdit && containsAny(toolName, "Write", "Edit", "MultiEdit") {
		fmt.Println(`{"continue": true}`)
		return
	}

	appendHookLog(fmt.Sprintf("%s - feedback-collector: processed\n", timestamp()))
	fmt.Println(`{"continue": true}`)
}

func cmdTmuxReady() {
	if os.Getenv("TMUX") == "" {
		return
	}

	windowID, _ := tmux.GetWindowID()
	if windowID == "" {
		windowID = "current"
	}

	_ = tmux.UpdateTmuxWindow("✓ ready", windowID, "stop-hook")
	appendHookLog(fmt.Sprintf("%s - tmux-ready: ready\n", timestamp()))

	// Surface any interactive loomie completions that arrived during this turn.
	// Writes a systemMessage into the UI if any matching completions are pending.
	cmdLoomieCheckCompletions()
}

func cmdTmuxNotification() {
	if os.Getenv("TMUX") == "" {
		return
	}

	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}

	message := input.Message
	windowID, _ := tmux.GetWindowID()

	if strings.Contains(message, "permission") || strings.Contains(message, "approval") {
		_ = tmux.UpdateTmuxWindow("⚠ APPROVE", windowID, "notification")
	} else if strings.Contains(message, "waiting for your input") {
		_ = tmux.UpdateTmuxWindow("waiting for input", windowID, "notification")
	} else {
		_ = tmux.UpdateTmuxWindow("notification", windowID, "notification")
	}

	appendHookLog(fmt.Sprintf("%s - tmux-notification: %s\n", timestamp(), message))
}

func cmdTmuxThinking() {
	if os.Getenv("TMUX") == "" {
		return
	}

	windowID, _ := tmux.GetWindowID()
	if windowID == "" {
		windowID = "current"
	}

	_ = tmux.UpdateTmuxWindow("⚡ thinking", windowID, "user-prompt-submit")
	appendHookLog(fmt.Sprintf("%s - tmux-thinking: thinking\n", timestamp()))
}

func timestamp() string {
	return time.Now().Format(time.RFC3339Nano)
}

func appendHookLog(msg string) {
	f, err := os.OpenFile("/tmp/tmux-hook.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(msg)
	_ = f.Close()
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
