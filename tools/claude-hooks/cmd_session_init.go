// cmd_session_init.go — Go port of session-start/session-initialization.js
//
// Replaces the Node.js hook that was being SIGKILLed under system load
// because Node cold-start + 8 require()s exceeded the 5s timeout. Go
// cold-start eliminates that bottleneck.
//
// FAF discipline: never panic, never block, never exit nonzero.
// Every external action is best-effort: bounded by a timeout and errors
// are logged via hooklog (or tmux-hook.log for consumer-facing lines)
// then swallowed.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	osexec "os/exec"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

// ---------- tracker file paths (vars so tests can override) ----------

var (
	siConversationsFile   = "/tmp/claude-conversations.json"
	siWindowConvsFile     = "/tmp/claude-window-conversations.json"
	siProcessRegistryFile = "/tmp/claude-process-registry.json"
)

// ---------- JSON shapes (field order stable via struct tags) ----------

// siConvEntry is the per-conversation record written to claude-conversations.json.
// Field order matches the JS shape exactly (json.MarshalIndent encodes in
// declaration order for structs).
type siConvEntry struct {
	ConversationID        string   `json:"conversationId"`
	ConversationStartTime int64    `json:"conversation_start_time"`
	CurrentWindow         string   `json:"current_window"`
	TotalOperations       int      `json:"total_operations"`
	LastActivity          int64    `json:"last_activity"`
	RecentSessionIDs      []string `json:"recent_session_ids"`
	WindowsUsed           []string `json:"windows_used"`
	Created               int64    `json:"created"`
	LastUpdate            int64    `json:"last_update"`
	TmuxSession           string   `json:"tmux_session"`
}

// siWindowStats is written to /tmp/tmux-session-stats-<windowId>.json.
type siWindowStats struct {
	StartTime      int64  `json:"startTime"`
	Operations     int    `json:"operations"`
	LastUpdate     int64  `json:"lastUpdate"`
	WindowID       string `json:"windowId"`
	ConversationID string `json:"conversationId"`
	SessionID      string `json:"sessionId"`
}

// siProcEntry is one entry under "processes" in claude-process-registry.json.
type siProcEntry struct {
	PID            int     `json:"pid"`
	Type           string  `json:"type"`
	ConversationID string  `json:"conversation_id"`
	Window         string  `json:"window"`
	TTY            *string `json:"tty"`
	StartTime      int64   `json:"start_time"`
	LastHeartbeat  int64   `json:"last_heartbeat"`
	CommandLine    string  `json:"command_line"`
	ParentPID      int     `json:"parent_pid"`
	SessionID      string  `json:"session_id"`
	RegisteredTime int64   `json:"registered_time"`
}

// siProcMeta holds the "metadata" sub-object.
type siProcMeta struct {
	LastCleanup  int64 `json:"last_cleanup"`
	LastUpdate   int64 `json:"last_update"`
	ProcessCount int   `json:"process_count"`
}

// siProcRegistry is the top-level shape of claude-process-registry.json.
type siProcRegistry struct {
	Processes map[string]siProcEntry `json:"processes"`
	Metadata  siProcMeta             `json:"metadata"`
}

// ---------- UUID regex ----------

var uuidRE = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// ---------- logging helpers ----------

// logTmux appends a consumer-facing line to /tmp/tmux-hook.log.
// Format: <ISO_MS_UTC> - SessionStart: <message>\n
// Matches JS: new Date().toISOString() which outputs e.g. "2026-06-20T07:35:30.802Z"
func logTmux(format string, args ...any) {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	msg := fmt.Sprintf(format, args...)
	line := ts + " - SessionStart: " + msg + "\n"
	f, err := os.OpenFile("/tmp/tmux-hook.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		hooklog.Writef("session-init: tmux-hook.log open error: %v", err)
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(line)
}

// ---------- window detection ----------

// detectWindowID returns the tmux window index for the current pane.
// Prefers TMUX_PANE-anchored query (accurate when multiple windows are
// open) and falls back to the active-window query.
func detectWindowID() string {
	ctx2s, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		out, err := osexec.CommandContext(ctx2s, "tmux", "display-message", "-t", pane, "-p", "#I").Output() //nolint:gosec
		if err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
	}

	// Fallback: active window (less accurate under multi-window setups,
	// but acceptable when TMUX_PANE is not set).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	out2, err2 := osexec.CommandContext(ctx2, "tmux", "display-message", "-p", "#I").Output() //nolint:gosec
	if err2 == nil {
		if v := strings.TrimSpace(string(out2)); v != "" {
			return v
		}
	}

	return ""
}

// tmuxSessionName returns the tmux session name (#S), best-effort.
func tmuxSessionName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------- conversation UUID extraction ----------

// extractConversationUUID returns the UUID from a transcript path like
// /home/.../.claude/projects/<slug>/<uuid>.jsonl, or "" if the basename
// is not a valid UUID. Pure function, safe to call in tests.
func extractConversationUUID(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(transcriptPath), ".jsonl")
	if uuidRE.MatchString(base) {
		return base
	}
	return ""
}

// ---------- new-vs-continuation decision (pure, tested) ----------

// decideNewConversation implements the JS isNewConversation logic.
// existing is the full loaded map from claude-conversations.json.
// convId is the extracted UUID ("" → always new).
// source is the hook source field (startup, clear, resume, compact, …).
// nowMs is the current epoch milliseconds (injectable for tests).
func decideNewConversation(existing map[string]siConvEntry, convId, source string, nowMs int64) bool {
	if convId == "" {
		return true
	}
	conv, found := existing[convId]
	if !found {
		return true
	}
	if source == "clear" || source == "startup" {
		return true
	}
	// resume / compact: new if last activity > 24 h ago
	const maxAgeMs = 24 * 60 * 60 * 1000
	if nowMs-conv.LastActivity > maxAgeMs {
		return true
	}
	return false
}

// ---------- file I/O helpers ----------

// loadConversations reads claude-conversations.json.
// Returns empty map on any error.
func loadConversations() map[string]siConvEntry {
	data, err := os.ReadFile(siConversationsFile)
	if err != nil {
		return map[string]siConvEntry{}
	}
	var m map[string]siConvEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]siConvEntry{}
	}
	return m
}

// saveConversations writes the map back with 2-space indent.
func saveConversations(m map[string]siConvEntry) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		hooklog.Writef("session-init: marshal conversations: %v", err)
		return
	}
	if err := os.WriteFile(siConversationsFile, b, 0644); err != nil {
		hooklog.Writef("session-init: write conversations: %v", err)
	}
}

// loadWindowConvs reads claude-window-conversations.json.
func loadWindowConvs() map[string]string {
	data, err := os.ReadFile(siWindowConvsFile)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]string{}
	}
	return m
}

// saveWindowConvs writes the window→convId map with 2-space indent.
func saveWindowConvs(m map[string]string) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(siWindowConvsFile, b, 0644)
}

// loadProcessRegistry reads claude-process-registry.json.
func loadProcessRegistry() siProcRegistry {
	data, err := os.ReadFile(siProcessRegistryFile)
	if err != nil {
		return siProcRegistry{
			Processes: map[string]siProcEntry{},
			Metadata:  siProcMeta{LastCleanup: time.Now().UnixMilli()},
		}
	}
	var r siProcRegistry
	if err := json.Unmarshal(data, &r); err != nil {
		return siProcRegistry{
			Processes: map[string]siProcEntry{},
			Metadata:  siProcMeta{LastCleanup: time.Now().UnixMilli()},
		}
	}
	if r.Processes == nil {
		r.Processes = map[string]siProcEntry{}
	}
	return r
}

// saveProcessRegistry writes the registry with 2-space indent.
func saveProcessRegistry(r siProcRegistry) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(siProcessRegistryFile, b, 0644)
}

// ---------- process helpers ----------

// isProcessAlive returns true when kill(pid, 0) succeeds (process exists).
func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// readCmdLine reads /proc/<pid>/cmdline and joins NUL-separated fields with spaces.
func readCmdLine(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	// cmdline is NUL-separated; trim trailing NUL first
	data = bytes.TrimRight(data, "\x00")
	parts := bytes.Split(data, []byte{0})
	strs := make([]string, 0, len(parts))
	for _, p := range parts {
		strs = append(strs, string(p))
	}
	return strings.Join(strs, " ")
}

// readParentPID reads the parent PID from /proc/<pid>/status.
func readParentPID(pid int) int {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if n, err := strconv.Atoi(fields[1]); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// ---------- process registry update ----------

// registerProcess registers the Claude CLI parent process in the registry
// and cleans up stale entries. Returns the count of stale processes removed.
func registerProcess(convId, windowId, sessionId string) int {
	reg := loadProcessRegistry()
	now := time.Now().UnixMilli()
	ppid := os.Getppid()
	pidStr := strconv.Itoa(ppid)

	existing, alreadyIn := reg.Processes[pidStr]
	if alreadyIn {
		existing.LastHeartbeat = now
		existing.ConversationID = convId
		existing.Window = windowId
		existing.SessionID = sessionId
		reg.Processes[pidStr] = existing
	} else {
		reg.Processes[pidStr] = siProcEntry{
			PID:            ppid,
			Type:           "claude",
			ConversationID: convId,
			Window:         windowId,
			TTY:            nil,
			StartTime:      now,
			LastHeartbeat:  now,
			CommandLine:    readCmdLine(ppid),
			ParentPID:      readParentPID(ppid),
			SessionID:      sessionId,
			RegisteredTime: now,
		}
	}

	// Cleanup: drop dead or stale (heartbeat > 120 s old) processes.
	const staleMs = 120_000
	cleaned := 0
	for k, p := range reg.Processes {
		if k == pidStr {
			continue
		}
		dead := !isProcessAlive(p.PID)
		stale := now-p.LastHeartbeat > staleMs
		if dead || stale {
			delete(reg.Processes, k)
			cleaned++
		}
	}

	reg.Metadata.LastCleanup = now
	reg.Metadata.LastUpdate = now
	reg.Metadata.ProcessCount = len(reg.Processes)
	saveProcessRegistry(reg)
	return cleaned
}

// ---------- conversation tracker update ----------

// updateConversation creates or updates the conversation entry.
// Returns the updated entry.
func updateConversation(convId, windowId, sessionId string, isNew bool) siConvEntry {
	convs := loadConversations()
	now := time.Now().UnixMilli()
	tmuxSess := tmuxSessionName()

	var entry siConvEntry
	if isNew {
		entry = siConvEntry{
			ConversationID:        convId,
			ConversationStartTime: now,
			CurrentWindow:         windowId,
			TotalOperations:       0,
			LastActivity:          now,
			RecentSessionIDs:      []string{sessionId},
			WindowsUsed:           []string{windowId},
			Created:               now,
			LastUpdate:            now,
			TmuxSession:           tmuxSess,
		}
	} else {
		entry = convs[convId]
		entry.LastActivity = now
		entry.LastUpdate = now
		// Window tracking
		if windowId != "" && entry.CurrentWindow != windowId {
			entry.CurrentWindow = windowId
			if !containsStr(entry.WindowsUsed, windowId) {
				entry.WindowsUsed = append(entry.WindowsUsed, windowId)
			}
		}
		// Prepend sessionId (unshift), deduplicate, keep last 5
		if sessionId != "" && !containsStr(entry.RecentSessionIDs, sessionId) {
			entry.RecentSessionIDs = append([]string{sessionId}, entry.RecentSessionIDs...)
			if len(entry.RecentSessionIDs) > 5 {
				entry.RecentSessionIDs = entry.RecentSessionIDs[:5]
			}
		}
		if tmuxSess != "" {
			entry.TmuxSession = tmuxSess
		}
	}

	convs[convId] = entry
	saveConversations(convs)

	// Update window→conv mapping
	wm := loadWindowConvs()
	if windowId != "" {
		wm[windowId] = convId
	}
	saveWindowConvs(wm)

	return entry
}

// updateWindowStats writes /tmp/tmux-session-stats-<windowId>.json.
func updateWindowStats(convId, windowId, sessionId string, startTime int64) {
	path := fmt.Sprintf("/tmp/tmux-session-stats-%s.json", windowId)

	// Preserve existing startTime if the file is present for the same convId.
	existingStart := startTime
	if data, err := os.ReadFile(path); err == nil {
		var s siWindowStats
		if err := json.Unmarshal(data, &s); err == nil && s.ConversationID == convId && s.StartTime > 0 {
			existingStart = s.StartTime
		}
	}

	convs := loadConversations()
	ops := 0
	if e, ok := convs[convId]; ok {
		ops = e.TotalOperations
	}

	stats := siWindowStats{
		StartTime:      existingStart,
		Operations:     ops,
		LastUpdate:     time.Now().UnixMilli(),
		WindowID:       windowId,
		ConversationID: convId,
		SessionID:      sessionId,
	}
	b, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0644)
}

// ---------- daemon management ----------

// ensureDaemon checks if a daemon process (identified by pidFile) is alive
// and optionally healthy. If not, it spawns `node <script> [args...]` as a
// detached process and writes the new pid to pidFile.
// healthCheck may be nil (no health test).
// env is extra env vars appended to the parent environment.
func ensureDaemon(pidFile, script string, scriptArgs []string, extraEnv map[string]string, healthCheck func() bool) {
	// Check if already running.
	if data, err := os.ReadFile(pidFile); err == nil {
		pidStr := strings.TrimSpace(string(data))
		if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
			if isProcessAlive(pid) {
				if healthCheck == nil || healthCheck() {
					return // already up and healthy
				}
				// Alive but stale: kill it and fall through to respawn.
				_ = syscall.Kill(pid, syscall.SIGTERM)
				hooklog.Writef("session-init: killed stale daemon pid=%d script=%s", pid, script)
			}
		}
		// Stale pid file — remove it.
		_ = os.Remove(pidFile)
	}

	// Spawn detached.
	nodeArgs := append([]string{script}, scriptArgs...)
	cmd := osexec.Command("node", nodeArgs...) //nolint:gosec
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Build env
	if len(extraEnv) > 0 {
		env := os.Environ()
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	if err := cmd.Start(); err != nil {
		hooklog.Writef("session-init: spawn daemon %s: %v", script, err)
		return
	}
	// Do NOT Wait() — detached.
	pid := cmd.Process.Pid
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0644)
	hooklog.Writef("session-init: started daemon pid=%d script=%s", pid, script)
}

func ensureInvestigationNotifier() {
	ensureDaemon(
		"/tmp/claude-investigation-notifier.pid",
		"/home/eric/.claude/hooks/lib/investigation-notifier-daemon.js",
		nil,
		map[string]string{"CLAUDE_INVESTIGATION_NOTIFIER": "1"},
		nil,
	)
}

func ensureEnvironmentalService() {
	healthCheck := func() bool {
		fi, err := os.Stat("/tmp/environmental-data.json")
		if err != nil {
			return false
		}
		return time.Since(fi.ModTime()) < 120*time.Second
	}
	ensureDaemon(
		"/tmp/claude-environmental-daemon.pid",
		"/home/eric/.claude/hooks/lib/environmental-intelligence-service.js",
		[]string{"start"},
		nil,
		healthCheck,
	)
}

// ---------- cleanup ----------

// killAllDaemons is the neutered daemon-manager behavior: pkill spinner-daemon
// and remove its state files (matching the JS which does exactly this).
func killAllDaemons() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = osexec.CommandContext(ctx, "pkill", "-9", "-f", "spinner-daemon.js").Run() //nolint:gosec
	_ = os.Remove("/tmp/claude-spinner-daemon.pid")
	_ = os.Remove("/tmp/claude-spinners.json")
}

// performDeepCleanup removes window-specific temp files (new-conversation path only).
func performDeepCleanup(windowId string) {
	if windowId == "" {
		return
	}
	// Pattern: /tmp/*-<windowId>-* matching claude-* or tmux-*
	ctx1, c1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c1()
	_ = osexec.CommandContext(ctx1, "sh", "-c", //nolint:gosec
		fmt.Sprintf(`find /tmp -maxdepth 1 -name "*-%s-*" -type f \( -name "claude-*" -o -name "tmux-*" \) -delete 2>/dev/null || true`, windowId),
	).Run()

	// Clean old status coordinator files (> 60 min)
	ctx2, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	_ = osexec.CommandContext(ctx2, "sh", "-c", //nolint:gosec
		`find /tmp -maxdepth 1 -name "claude-status-coordinator-*" -mmin +60 -delete 2>/dev/null || true`,
	).Run()
}

// ---------- MCP registration ----------

// mcpRegister POSTs agent registration to the MCP endpoint (currently DOWN;
// best-effort, 500 ms cap).
func mcpRegister(convId, windowId, sessionId string, ppid int, cwd string) {
	gitBranch := ""
	if cwd != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := osexec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output() //nolint:gosec
		if err == nil {
			gitBranch = strings.TrimSpace(string(out))
		}
	}

	tmuxSess := tmuxSessionName()
	tmuxPane := os.Getenv("TMUX_PANE")

	payload := map[string]any{
		"conversation_id": convId,
		"tmux_session":    tmuxSess,
		"tmux_window":     windowId,
		"tmux_pane":       tmuxPane,
		"pid":             ppid,
		"cwd":             cwd,
		"git_branch":      gitBranch,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Post("http://127.0.0.1:3001/api/agent/register", "application/json", bytes.NewReader(body))
	if err != nil {
		logTmux("MCP registration unavailable: %s", err.Error())
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logTmux("MCP registration failed (%d)", resp.StatusCode)
	} else {
		logTmux("Agent registered with MCP: %s", convId)
	}
}

// ---------- duration formatter ----------

func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	} else if ms < 60000 {
		return fmt.Sprintf("%ds", ms/1000)
	}
	mins := ms / 60000
	secs := (ms % 60000) / 1000
	return fmt.Sprintf("%dm %ds", mins, secs)
}

// ---------- helper ----------

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// ---------- main entry point ----------

// cmdSessionInit is the Go port of session-initialization.js.
func cmdSessionInit() {
	// Guard: only meaningful inside tmux.
	if os.Getenv("TMUX") == "" {
		return
	}

	// Parse stdin.
	in, _ := readStdinForHook()
	sessionId := in.SessionID
	transcriptPath := in.TranscriptPath
	source := in.Source

	// Post-compaction dispatch-state snapshot: fires on every source=="compact"
	// invocation regardless of which branch below this session falls into
	// (new/continuation/fallback), so defer it up front rather than threading
	// it through every return path. See emitCompactDispatchState doc comment.
	defer emitCompactDispatchState(source, in.EffectiveCWD())

	// Detect window and conversation.
	windowId := detectWindowID()
	convId := extractConversationUUID(transcriptPath)

	// Ensure daemons are up (best-effort, before any log so they start ASAP).
	ensureInvestigationNotifier()
	ensureEnvironmentalService()

	// Entry log.
	logTmux("source=%s, conversation_id=%s, session_id=%s, window=%s",
		source, convId, sessionId, windowId)

	// Fallback path: no valid conversation UUID.
	if convId == "" {
		logTmux("Unable to extract conversation ID from %s, treating as fallback session", transcriptPath)
		handleFallbackSession(sessionId, windowId, source)
		return
	}

	// Decide new vs continuation.
	convMap := loadConversations()
	nowMs := time.Now().UnixMilli()
	isNew := decideNewConversation(convMap, convId, source, nowMs)

	cwd := in.EffectiveCWD()
	ppid := os.Getppid()

	if isNew {
		// Kill spinner daemon, deep-clean window temp files.
		killAllDaemons()
		performDeepCleanup(windowId)

		logTmux("Initializing NEW conversation %s in window %s", convId, windowId)

		conv := updateConversation(convId, windowId, sessionId, true)
		cleaned := registerProcess(convId, windowId, sessionId)
		logTmux("Process registry - PID %d linked to conversation %s, cleaned %d stale processes",
			ppid, convId, cleaned)
		updateWindowStats(convId, windowId, sessionId, nowMs)

		logTmux("New conversation %s initialized successfully in window %s (%d ops)",
			convId, windowId, conv.TotalOperations)

		mcpRegister(convId, windowId, sessionId, ppid, cwd)
	} else {
		logTmux("Continuing conversation %s in window %s", convId, windowId)

		startMs := int64(0)
		if e, ok := convMap[convId]; ok {
			startMs = e.ConversationStartTime
		}

		conv := updateConversation(convId, windowId, sessionId, false)
		cleaned := registerProcess(convId, windowId, sessionId)
		logTmux("Process registry - PID %d linked to conversation %s, cleaned %d stale processes",
			ppid, convId, cleaned)
		updateWindowStats(convId, windowId, sessionId, startMs)

		elapsed := formatDuration(nowMs - startMs)
		logTmux("Conversation %s continued successfully (%d ops, %s elapsed)",
			convId, conv.TotalOperations, elapsed)

		mcpRegister(convId, windowId, sessionId, ppid, cwd)
	}
}

// handleFallbackSession writes a minimal window-stats file when no valid
// conversation UUID can be extracted (matches JS handleFallbackSession).
func handleFallbackSession(sessionId, windowId, source string) {
	logTmux("Fallback session handling for %s in window %s", sessionId, windowId)

	now := time.Now().UnixMilli()
	stats := siWindowStats{
		StartTime:  now,
		Operations: 0,
		LastUpdate: now,
		WindowID:   windowId,
		SessionID:  sessionId,
	}
	if windowId != "" {
		b, err := json.MarshalIndent(stats, "", "  ")
		if err == nil {
			path := fmt.Sprintf("/tmp/tmux-session-stats-%s.json", windowId)
			_ = os.WriteFile(path, b, 0644)
		}
	}

	logTmux("Fallback session %s handled", sessionId)
	_ = source // used in JS for cleanup context but not needed here
}

// ---------- post-compaction dispatch-state snapshot ----------
//
// Replaces the removed "compact freeze" design (cmd_compact_freeze.go,
// commit c3dfe6a, reverted 2026-07-11): that design armed a per-session
// counter on PreCompact and three global rules exit-2-blocked the FIRST
// post-compaction Agent/Task/Workflow dispatch with a lecture telling the
// model to go check git status / TaskList itself. That burns a full failed
// dispatch plus an identical retry to demand verification the hook could
// have just performed. The replacement below does the mechanical part
// itself — git status counts — and injects it as SessionStart context
// instead of blocking anything. Blocks stay reserved for mechanically
// detected hazards (see the isolation/git-whole-tree rules in
// configs/global-hook-rules.json); this is advisory, not enforcement.

// compactDispatchStateTimeout bounds the whole snapshot (rev-parse +
// status --porcelain, run sequentially against one repo). Either call
// timing out or erroring fails soft to no output — this must never delay
// or block SessionStart.
const compactDispatchStateTimeout = 1 * time.Second

// compactDispatchStateBlock builds the "## DISPATCH STATE (live at
// compaction)" context block for one repo: the repo containing the
// session's cwd, per the hook payload.
//
// Scope is deliberately narrow. The only OTHER session-relevant repos
// worth adding would come from this binary's own session-state tracking
// (siConversationsFile / siProcessRegistryFile / plan-resume's
// plans-<convId>.json) — but as of this writing none of that state tracks
// a *set of repos touched by the session*; it all keys on
// conversation/window/plan IDs within a single project. So today this
// function only ever reports the cwd repo. That is an acceptable, honest
// result — NOT a reason to fall back to scanning ~/projects, which the
// operator has explicitly ruled out (unscoped git-status sweeps burn
// wall-clock across unrelated repos and report on work nobody dispatched
// into). If session-relevant multi-repo tracking is added later, extend
// the repo list built here instead.
//
// Returns "" (fail-soft) when cwd is empty, isn't inside a git repo, or
// either git call errors/times out.
func compactDispatchStateBlock(cwd string) string {
	if cwd == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), compactDispatchStateTimeout)
	defer cancel()

	rootOut, err := osexec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	repoRoot := strings.TrimSpace(string(rootOut))
	if repoRoot == "" {
		return ""
	}

	statusOut, err := osexec.CommandContext(ctx, "git", "-C", repoRoot, "status", "--porcelain").Output() //nolint:gosec
	if err != nil {
		return ""
	}

	dirty := 0
	for _, line := range strings.Split(string(statusOut), "\n") {
		if strings.TrimSpace(line) != "" {
			dirty++
		}
	}

	repoName := filepath.Base(repoRoot)
	var b strings.Builder
	b.WriteString("## DISPATCH STATE (live at compaction)\n")
	fmt.Fprintf(&b, "- %s (%s): %d dirty path(s) [git status --porcelain]\n", repoName, repoRoot, dirty)
	b.WriteString("\nBefore re-dispatching from a compacted summary: check TaskList/agent roster for existing owners; one BINDING design doc per feature.")
	return b.String()
}

// buildCompactDispatchStateOutput returns the ready-to-print
// hookSpecificOutput JSON line for SessionStart, or "" when there's
// nothing to say: source isn't "compact", or compactDispatchStateBlock
// itself came back empty (non-repo cwd, git error/timeout). Split out
// from emitCompactDispatchState so the gating logic is testable without
// capturing stdout.
func buildCompactDispatchStateOutput(source, cwd string) string {
	if source != "compact" {
		return ""
	}
	block := compactDispatchStateBlock(cwd)
	if block == "" {
		return ""
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": block,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// emitCompactDispatchState prints the dispatch-state snapshot to stdout
// (Claude Code folds SessionStart hookSpecificOutput.additionalContext
// into the fresh post-compaction context window) when source=="compact"
// and a snapshot was buildable. No-op otherwise — matches every other
// session-init path's FAF discipline (never panic, never block).
func emitCompactDispatchState(source, cwd string) {
	line := buildCompactDispatchStateOutput(source, cwd)
	if line == "" {
		return
	}
	fmt.Println(line)
}
