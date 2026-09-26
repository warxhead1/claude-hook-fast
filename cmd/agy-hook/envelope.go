package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// gitWorktreeRe extracts the primary repo path from a worktree's `.git`
// gitdir pointer, e.g. "/home/eric/projects/foo/.git/worktrees/bar" ->
// group 1 = "/home/eric/projects/foo". Port of the Python script's
// `re.match(r"(.*)/\.git/worktrees/", gitdir)`.
var gitWorktreeRe = regexp.MustCompile(`(.*)/\.git/worktrees/`)

// pyBasename mirrors Python's os.path.basename: the tail after the last
// "/", which is "" for an empty string or a path ending in "/". Go's
// filepath.Base normalizes ("" -> ".", trailing slash -> last segment
// anyway) in ways that would silently diverge from the Python script's
// behavior at the empty-path fallback below.
func pyBasename(s string) string {
	if s == "" {
		return ""
	}
	idx := strings.LastIndex(s, "/")
	return s[idx+1:]
}

// repoSplit is a byte-for-byte port of the Python script's repo_split:
// walk up from cwd looking for a .git marker. A .git directory means cwd
// (or an ancestor) IS the primary checkout. A .git file is a worktree
// pointer; when its gitdir matches ".../.git/worktrees/", the primary
// repo's basename is the project and the worktree directory's own
// basename is the worktree. Falls back to the basename of the ORIGINAL
// (pre-resolution) path argument if no .git is ever found — matching the
// Python script's `os.path.basename(path or "") or "unknown"`, which
// deliberately does NOT fall back to cwd even though the walk itself
// does.
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

// firstWorkspacePath mirrors `(payload.get("workspacePaths") or
// [os.getcwd()])[0]`'s workspacePaths half: returns "" when the key is
// absent, not a list, or an empty list, leaving the os.Getwd() fallback
// to the caller.
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

// buildEnvelope is a port of the Python script's envelope(): it does NOT
// strip empty-string fields — that happens once, explicitly, via
// compactEnvelope, after any subcommand-specific fields (termination_
// reason, error) have been added, mirroring the Python publish()
// function's filtering happening at send time on a still-unfiltered env.
func buildEnvelope(payload orderedMap, event, toolName, toolSummary string) (env map[string]any, project string) {
	cwd := firstWorkspacePath(payload)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	proj, wt := repoSplit(cwd)
	sid := payload.GetString("conversationId")
	env = map[string]any{
		"ts":              pyTimestamp(),
		"event":           event,
		"agent":           "antigravity",
		"agent_kind":      "antigravity-cli",
		"agent_id":        sid,
		"session_id":      sid,
		"project":         proj,
		"worktree":        wt,
		"display":         displayName(proj, wt),
		"cwd":             cwd,
		"model":           payload.GetString("modelName"),
		"source_kind":     "antigravity-cli",
		"transcript_path": payload.GetString("transcriptPath"),
		"pane_id":         firstNonEmptyEnv("ZELLIJ_PANE_ID", "TMUX_PANE"),
		"session_name":    os.Getenv("ZELLIJ_SESSION_NAME"),
	}
	if toolName != "" {
		env["tool_name"] = toolName
	}
	if toolSummary != "" {
		env["tool_summary"] = toolSummary
	}
	return env, proj
}

func firstNonEmptyEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// compactEnvelope removes keys whose value is "" or nil, matching the
// Python publish() filter `{k: v for k, v in env.items() if v not in
// ("", None)}`.
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

// toolInfo is a port of the Python script's tool_info(): the primary
// command/description summary preferred over a raw json.dumps(args)
// fallback, with an appended " [error: ...]" suffix when the payload
// carries a truthy error.
func toolInfo(payload orderedMap) (name, summary string) {
	var tc orderedMap
	if v, ok := payload.Get("toolCall"); ok {
		tc, _ = v.(orderedMap)
	}
	name = tc.GetString("name")

	var args orderedMap
	if v, ok := tc.Get("args"); ok {
		args, _ = v.(orderedMap)
	}
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

func jsonMarshal(env map[string]any) (string, error) {
	b, err := json.Marshal(compactEnvelope(env))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
