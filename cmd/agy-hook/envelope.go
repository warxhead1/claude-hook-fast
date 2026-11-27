package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

// gitWorktreeRe extracts the primary repo path from a worktree's `.git`
// gitdir pointer, e.g. "/home/eric/projects/foo/.git/worktrees/bar" ->
// group 1 = "/home/eric/projects/foo". Port of the Python script's
// `re.match(r"(.*)/\.git/worktrees/", gitdir)`.
var gitWorktreeRe = regexp.MustCompile(`(.*)/\.git/worktrees/`)

// pyBasename mirrors Python's os.path.basename: the tail after the last
// "/", which is "" for an empty string or a path ending in "/".
func pyBasename(s string) string {
	if s == "" {
		return ""
	}
	idx := strings.LastIndex(s, "/")
	return s[idx+1:]
}

// repoSplit walks up from path looking for a .git marker. A .git directory
// means path (or an ancestor) is the primary checkout. A .git file is a
// worktree pointer; when its gitdir matches ".../.git/worktrees/", the
// primary repo's basename is the project and the worktree directory's own
// basename is the worktree. Falls back to the basename of the original
// path argument if no .git is found.
func repoSplit(path string) (project, worktree string) {
	walk := path
	if walk == "" {
		if wd, err := os.Getwd(); err == nil {
			walk = wd
		}
	}
	if resolved, err := filepath.EvalSymlinks(walk); err == nil {
		walk = resolved
	}
	p := walk
	for {
		g := filepath.Join(p, ".git")
		info, err := os.Stat(g)
		if err == nil {
			if info.IsDir() {
				return pyBasename(p), ""
			}
			// .git is a file: a worktree pointer.
			if data, rerr := os.ReadFile(g); rerr == nil {
				content := strings.TrimSpace(string(data))
				content = strings.TrimPrefix(content, "gitdir:")
				content = strings.TrimSpace(content)
				if m := gitWorktreeRe.FindStringSubmatch(content); m != nil {
					return pyBasename(m[1]), pyBasename(p)
				}
			}
			return pyBasename(p), ""
		}
		parent := filepath.Dir(p)
		if parent == p {
			b := pyBasename(path)
			if b == "" {
				return "unknown", ""
			}
			return b, ""
		}
		p = parent
	}
}

// displayName mirrors `"%s [%s]" % (project, wt) if wt else project`.
func displayName(project, worktree string) string {
	if worktree == "" {
		return project
	}
	return project + " [" + worktree + "]"
}

// pyTimestamp reproduces the Python script's _ts(): a single clock read
// split into whole seconds and a microsecond fraction, formatted as
// RFC3339 with exactly 6 fractional digits and a literal "Z".
func pyTimestamp() string {
	ns := time.Now().UnixNano()
	sec := ns / 1_000_000_000
	frac := (ns / 1000) % 1_000_000
	return fmt.Sprintf("%s.%06dZ", time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05"), frac)
}

// firstWorkspacePath returns the first element of payload.workspacePaths,
// or "" when absent or empty.
func firstWorkspacePath(payload orderedMap) string {
	v, ok := payload.Get("workspacePaths")
	if !ok {
		return ""
	}
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	s, _ := arr[0].(string)
	return s
}

func effectiveCWD(payload orderedMap) string {
	if cwd := payload.GetString("cwd"); cwd != "" {
		return cwd
	}
	if cwd := firstWorkspacePath(payload); cwd != "" {
		return cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

// agyPaneID returns the pane identifier Hearth's pane arbiter keys on:
// zellij:<session>:<pane> (or bare ZELLIJ_PANE_ID when session name is unset),
// raw tmux "%N", or "orca:<handle>" from ORCA_TERMINAL_HANDLE.
func agyPaneID() string {
	if s, p := os.Getenv("ZELLIJ_SESSION_NAME"), os.Getenv("ZELLIJ_PANE_ID"); s != "" && p != "" {
		return "zellij:" + s + ":" + p
	}
	if p := os.Getenv("ZELLIJ_PANE_ID"); p != "" {
		return p
	}
	if p := os.Getenv("TMUX_PANE"); p != "" {
		return p
	}
	if h := event.OrcaTerminalHandle(); h != "" {
		return "orca:" + h
	}
	return ""
}

// stampOrcaIdentity adds the Orca managed_by/terminal/worktree/tab fields to
// env when the hook runs inside an Orca-managed PTY (matching Claude Code's
// stampOrcaIdentity in tools/claude-hooks/cmd_nbus.go).
func stampOrcaIdentity(env map[string]any) {
	if h := event.OrcaTerminalHandle(); h != "" {
		env["managed_by"] = "orca"
		env["orca_terminal"] = h
		env["orca_worktree_id"] = os.Getenv("ORCA_WORKTREE_ID")
		env["orca_tab_id"] = os.Getenv("ORCA_TAB_ID")
	} else if os.Getenv("ORCA_HISTFILE") != "" {
		env["managed_by"] = "orca"
	}
}

// buildEnvelope builds the base nervous-bus envelope for agy lifecycle
// and activity events, including Orca terminal identity and qualified pane.
func buildEnvelope(payload orderedMap, eventName, toolName, toolSummary string) (env map[string]any, project string) {
	cwd := effectiveCWD(payload)
	proj, wt := repoSplit(cwd)
	sid := payload.GetString("conversationId")
	env = map[string]any{
		"ts":                pyTimestamp(),
		"event":             eventName,
		"agent":             "antigravity",
		"agent_kind":        "antigravity-cli",
		"agent_id":          sid,
		"session_id":        sid,
		"conversation_id":   sid,
		"project":           proj,
		"worktree":          wt,
		"display":           displayName(proj, wt),
		"cwd":               cwd,
		"model":             payload.GetString("modelName"),
		"source_kind":       "antigravity-cli",
		"transcript_path":   payload.GetString("transcriptPath"),
		"pane_id":           agyPaneID(),
		"pane_id_qualified": event.PaneIDQualified(),
		"session_name":      os.Getenv("ZELLIJ_SESSION_NAME"),
	}
	stampOrcaIdentity(env)
	if toolName != "" {
		env["tool_name"] = toolName
	}
	if toolSummary != "" {
		env["tool_summary"] = toolSummary
	}
	return env, proj
}

// compactEnvelope removes keys whose value is "" or nil.
func compactEnvelope(env map[string]any) map[string]any {
	for k, v := range env {
		if v == nil {
			delete(env, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(env, k)
		}
	}
	return env
}

func toolCallMap(payload orderedMap) orderedMap {
	if v, ok := payload.Get("toolCall"); ok {
		tc, _ := v.(orderedMap)
		return tc
	}
	return nil
}

func toolCallID(payload orderedMap) string {
	return toolCallMap(payload).GetString("id")
}

func toolCallArgs(payload orderedMap) orderedMap {
	tc := toolCallMap(payload)
	if v, ok := tc.Get("args"); ok {
		args, _ := v.(orderedMap)
		return args
	}
	return nil
}

// toolInfo extracts the tool name and a bounded summary (preferring
// CommandLine/command/description over raw json.dumps(args)), appending
// " [error: ...]" when the payload carries a truthy error.
func toolInfo(payload orderedMap) (name, summary string) {
	tc := toolCallMap(payload)
	name = tc.GetString("name")
	args := toolCallArgs(payload)
	for _, key := range []string{"CommandLine", "command", "description"} {
		if v, ok := args.Get(key); ok {
			if s, ok2 := v.(string); ok2 && s != "" {
				summary = truncateRunes(s, 200)
				break
			}
		}
	}
	if summary == "" && len(args) > 0 {
		summary = truncateRunes(pyDumps(args), 200)
	}
	if errv, ok := payload.Get("error"); ok && pythonTruthy(errv) {
		summary = truncateRunes(summary+" [error: "+pyStr(errv)+"]", 200)
	}
	return name, summary
}

// extractAgyResources extracts literal file resource touches for agy's
// file-oriented tools (read_file, write_file, edit_file, view_file),
// matching the {ref, intent} shape in bus.agent.activity.v1.
func extractAgyResources(toolName, cwd string, args orderedMap) []map[string]string {
	if toolName == "" || len(args) == 0 {
		return nil
	}
	var intent string
	switch toolName {
	case "read_file", "view_file", "ReadFile":
		intent = "read"
	case "write_file", "edit_file", "WriteFile", "EditFile", "replace_file_content", "multi_replace_file_content":
		intent = "write"
	default:
		return nil
	}
	var p string
	for _, k := range []string{"Path", "TargetFile", "file_path", "path"} {
		if s := args.GetString(k); s != "" {
			p = s
			break
		}
	}
	if p == "" || strings.ContainsAny(p, "*?$~`") {
		return nil
	}
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	if len(p) > 1024 {
		return nil
	}
	return []map[string]string{{"ref": "file:" + p, "intent": intent}}
}

func jsonMarshal(env map[string]any) (string, error) {
	b, err := json.Marshal(compactEnvelope(env))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
