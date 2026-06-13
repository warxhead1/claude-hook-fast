package tmux

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func GetWindowID() (string, error) {
	if os.Getenv("TMUX") == "" {
		return "", nil
	}

	ppid := os.Getppid()
	for i := 0; i < 10; i++ {
		tty, err := getProcessTTY(ppid)
		if err != nil || tty == "" {
			ppid2, err := getParentPID(ppid)
			if err != nil || ppid2 == 0 || ppid2 == ppid {
				break
			}
			ppid = ppid2
			continue
		}

		windowID := findTmuxWindowByTTY(tty)
		if windowID != "" {
			return windowID, nil
		}

		ppid2, err := getParentPID(ppid)
		if err != nil || ppid2 == 0 || ppid2 == ppid {
			break
		}
		ppid = ppid2
	}
	return "", nil
}

func getProcessTTY(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	tty := strings.TrimSpace(string(out))
	if tty == "?" || tty == "??" || tty == "" {
		return "", nil
	}
	if !strings.HasPrefix(tty, "/dev/") {
		tty = "/dev/" + tty
	}
	return tty, nil
}

func getParentPID(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	ppidStr := strings.TrimSpace(string(out))
	if ppidStr == "" || ppidStr == "1" {
		return 0, nil
	}
	ppid, err := strconv.Atoi(ppidStr)
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

func findTmuxWindowByTTY(tty string) string {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{window_index}:#{pane_tty}").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && parts[1] == tty {
			return parts[0]
		}
	}
	return ""
}

type Priority int

const (
	PriorityIdle Priority = iota
	PriorityAchievement
	PriorityToolPreparing
	PriorityToolWorking
	PriorityToolSuccess
	PriorityToolError
	PriorityApprovalPending
	PriorityApprovalDenied
	PrioritySystemError
)

type StatusState struct {
	Text      string   `json:"text"`
	Priority  Priority `json:"priority"`
	Timestamp int64    `json:"timestamp"`
	SessionID string   `json:"sessionId"`
	WindowID  string   `json:"windowId"`
	Source    string   `json:"source"`
}

func GetSessionID() string {
	if os.Getenv("TMUX") == "" {
		return fmt.Sprintf("claude-%d-%d", os.Getppid(), os.Getpid())
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
	if err != nil {
		return fmt.Sprintf("claude-%d-%d", os.Getppid(), os.Getpid())
	}
	return strings.TrimSpace(string(out))
}

func getStatusFile(sessionID string) string {
	safeID := strings.ReplaceAll(sessionID, "/", "_")
	return fmt.Sprintf("/tmp/claude-status-coordinator-%s.json", safeID)
}

func LoadCurrentStatus() *StatusState {
	sessionID := GetSessionID()
	filename := getStatusFile(sessionID)
	data, err := os.ReadFile(filename)
	if err != nil {
		return &StatusState{Priority: PriorityIdle, Source: "init"}
	}
	var status StatusState
	if err := json.Unmarshal(data, &status); err != nil {
		return &StatusState{}
	}
	return &status
}

func SaveCurrentStatus(status *StatusState) error {
	sessionID := GetSessionID()
	filename := getStatusFile(sessionID)
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

func UpdateTmuxWindow(text string, windowID string, source string) error {
	if os.Getenv("TMUX") == "" {
		return nil
	}

	var displayText string
	if strings.Contains(text, "APPROVE") || strings.Contains(text, "🚨") {
		displayText = "⚠ APPROVE"
	} else {
		icon := "⚡"
		if strings.HasPrefix(text, "✓") || strings.Contains(text, "ready") {
			icon = "✓"
		} else if strings.HasPrefix(text, "✗") || strings.Contains(text, "error") {
			icon = "✗"
		}
		focus := getFocusName()
		if focus == "" {
			focus = "claude"
		}
		displayText = fmt.Sprintf("%s %s", focus, icon)
	}

	displayText = strings.ReplaceAll(displayText, `"`, `\"`)
	displayText = strings.ReplaceAll(displayText, "'", "\\'")

	if windowID != "" && windowID != "current" {
		sessionName := ""
		out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
		if err == nil {
			sessionName = strings.TrimSpace(string(out))
		}
		target := windowID
		if sessionName != "" {
			target = fmt.Sprintf("%s:%s", sessionName, windowID)
		}
		_, _ = exec.Command("tmux", "rename-window", "-t", target, displayText).Output()
	} else {
		_, _ = exec.Command("tmux", "rename-window", displayText).Output()
	}
	return nil
}

func getFocusName() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude"
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude/active-context.json"))
	if err != nil {
		return ""
	}
	var ctx struct {
		Context string `json:"context"`
	}
	if err := json.Unmarshal(data, &ctx); err != nil {
		return ""
	}
	return ctx.Context
}

func UpdateToolPreparing(toolName string) error {
	status := &StatusState{
		Text:      FormatToolStatus(toolName),
		Priority:  PriorityToolPreparing,
		Timestamp: time.Now().UnixMilli(),
		SessionID: GetSessionID(),
		Source:    "tmux-preparing",
	}
	if err := SaveCurrentStatus(status); err != nil {
		return err
	}
	windowID, _ := GetWindowID()
	return UpdateTmuxWindow(status.Text, windowID, "tmux-preparing")
}

func FormatToolStatus(toolName string) string {
	switch toolName {
	case "Bash":
		return "⚡ bash"
	case "Read":
		return "📖 read"
	case "Write":
		return "✏️ write"
	case "Edit":
		return "✏️ edit"
	case "Grep":
		return "🔍 grep"
	case "Glob":
		return "📂 glob"
	case "TodoWrite":
		return "📝 todo"
	default:
		return "⚡ " + strings.ToLower(toolName)
	}
}
