package main

// coord_verdict.go — landing verdict for worker_done wakes
// (docs/coordinator-inbox.md, component D): where the lane's branch is, whether
// it is pushed, whether it merges cleanly into the integration branch, what
// it touched, and which changed files fall outside its declared FILE SCOPE.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const coordGitTimeout = 20 * time.Second

var coordTestPathRE = regexp.MustCompile(`(_test\.go|test_.*\.py|/Tests?/|\.spec\.)`)

func coordGit(dir string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), coordGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if err != nil {
		if ctx.Err() != nil {
			return out.String(), code, fmt.Errorf("git %s timed out after %s", args[0], coordGitTimeout)
		}
		if code == 0 || strings.TrimSpace(errb.String()) != "" {
			return out.String(), code, fmt.Errorf("git %s: %s", args[0], coordTrunc(strings.TrimSpace(errb.String()), 200))
		}
	}
	return out.String(), code, nil
}

// coordWorkerLoc resolves a dispatch to its worktree path and branch.
func coordWorkerLoc(ctx context.Context, cfg coordConfig, dispatchID string) (path, branch string, err error) {
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "worker-show", "--dispatch", dispatchID, "--json")
	if err != nil && len(out) == 0 {
		return "", "", fmt.Errorf("worker-show failed: %v", err)
	}
	var ws struct {
		OK     bool `json:"ok"`
		Result struct {
			Worker struct {
				WorktreeID string `json:"worktreeId"`
			} `json:"worker"`
			Terminal struct {
				WorktreePath string `json:"worktreePath"`
				Branch       string `json:"branch"`
			} `json:"terminal"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &ws) != nil || !ws.OK {
		return "", "", fmt.Errorf("worker-show returned no worker for %s", dispatchID)
	}
	path = ws.Result.Terminal.WorktreePath
	if path == "" {
		if _, after, ok := strings.Cut(ws.Result.Worker.WorktreeID, "::"); ok {
			path = after
		}
	}
	if path == "" {
		return "", "", fmt.Errorf("no worktree path for dispatch %s", dispatchID)
	}
	return path, strings.TrimPrefix(ws.Result.Terminal.Branch, "refs/heads/"), nil
}

// coordTaskSpec returns the spec text of a task from task-list.
func coordTaskSpec(ctx context.Context, cfg coordConfig, runID, taskID string) string {
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "task-list", "--run", runID, "--json")
	if err != nil && len(out) == 0 {
		return ""
	}
	var tl struct {
		Result struct {
			Tasks []struct {
				ID   string `json:"id"`
				Spec string `json:"spec"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &tl) != nil {
		return ""
	}
	for _, t := range tl.Result.Tasks {
		if t.ID == taskID {
			return t.Spec
		}
	}
	return ""
}

var (
	coordScopeLineRE = regexp.MustCompile(`^\s*(?:[-*]\s*)?FILE SCOPE\b(.*)$`)
	coordSentenceRE  = regexp.MustCompile(`\.\s+[A-Z]|\s+ONLY\b`)
	coordParenRE     = regexp.MustCompile(`\([^)]*\)`)
)

// coordScopeGlobs extracts path globs from the first real FILE SCOPE line of a
// task spec. found reports whether such a line exists at all.
func coordScopeGlobs(spec string) (globs []string, found bool) {
	for _, line := range strings.Split(spec, "\n") {
		m := coordScopeLineRE.FindStringSubmatch(line)
		if m == nil || strings.Contains(m[1], "bounds what you") {
			continue
		}
		found = true
		s := coordParenRE.ReplaceAllString(m[1], " ")
		s = strings.TrimLeft(strings.TrimSpace(s), ":")
		if loc := coordSentenceRE.FindStringIndex(s); loc != nil {
			s = s[:loc[0]]
		}
		for _, tok := range coordSplitBraceAware(s) {
			tok = strings.Trim(tok, "`;,. ")
			if tok != "" && strings.ContainsAny(tok, "/*.") {
				globs = append(globs, tok)
			}
		}
		return globs, found
	}
	return nil, false
}

// coordSplitBraceAware splits on commas and whitespace outside {...}.
func coordSplitBraceAware(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '{':
			depth++
			cur.WriteRune(r)
		case r == '}':
			if depth > 0 {
				depth--
			}
			cur.WriteRune(r)
		case depth == 0 && (r == ',' || r == ' ' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func coordGlobRegexp(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '{':
			depth++
			b.WriteString("(?:")
		case c == '}' && depth > 0:
			depth--
			b.WriteString(")")
		case c == ',' && depth > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if !strings.HasSuffix(g, "**") && !strings.ContainsAny(g[strings.LastIndex(g, "/")+1:], "*?") {
		b.WriteString("(?:/.*)?")
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

func coordOutOfScope(files, globs []string) []string {
	var res []*regexp.Regexp
	for _, g := range globs {
		if re := coordGlobRegexp(g); re != nil {
			res = append(res, re)
		}
	}
	var out []string
	for _, f := range files {
		ok := false
		for _, re := range res {
			if re.MatchString(f) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, f)
		}
	}
	return out
}

func coordTopDirs(files []string, n int) string {
	cnt := map[string]int{}
	for _, f := range files {
		d := "."
		if i := strings.LastIndexByte(f, '/'); i >= 0 {
			d = f[:i]
			if p := strings.Split(d, "/"); len(p) > 2 {
				d = strings.Join(p[:2], "/")
			}
		}
		cnt[d]++
	}
	dirs := make([]string, 0, len(cnt))
	for d := range cnt {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if cnt[dirs[i]] != cnt[dirs[j]] {
			return cnt[dirs[i]] > cnt[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	if len(dirs) > n {
		dirs = dirs[:n]
	}
	parts := make([]string, len(dirs))
	for i, d := range dirs {
		parts[i] = fmt.Sprintf("%s (%d)", d, cnt[d])
	}
	return strings.Join(parts, ", ")
}

// coordVerdict builds the landing verdict text for a worker_done message. It
// never returns an error: failures become "verdict unavailable: <reason>".
func coordVerdict(ctx context.Context, cfg coordConfig, runID string, m coordMsg, integration string) string {
	p := m.payload()
	if p.DispatchID == "" {
		return "verdict unavailable: message payload has no dispatchId"
	}
	path, branch, err := coordWorkerLoc(ctx, cfg, p.DispatchID)
	if err != nil {
		return "verdict unavailable: " + err.Error()
	}
	if st, serr := os.Stat(path); serr != nil || !st.IsDir() {
		return "verdict unavailable: worktree " + path + " is not on disk"
	}
	head, _, err := coordGit(path, "rev-parse", "HEAD")
	if err != nil {
		return "verdict unavailable: " + err.Error()
	}
	head = strings.TrimSpace(head)
	if branch == "" {
		if b, _, e := coordGit(path, "rev-parse", "--abbrev-ref", "HEAD"); e == nil {
			branch = strings.TrimSpace(b)
		}
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("worktree %s | branch %s | head %s", path, branch, coordShort(head)))
	lines = append(lines, "pushed: "+coordPushState(path, branch, head))

	if integration == "" {
		lines = append(lines, "merge check, diff, tests, scope: skipped (no integration_branch in standing orders)")
		return strings.Join(lines, "\n")
	}
	ref := integration
	if _, _, e := coordGit(path, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); e != nil {
		ref = "origin/" + integration
	}
	numstat, _, err := coordGit(path, "diff", "--numstat", ref+"...HEAD")
	if err != nil {
		lines = append(lines, "diff vs "+integration+": unavailable: "+err.Error())
		return strings.Join(lines, "\n")
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(numstat), "\n") {
		if f := strings.Split(l, "\t"); len(f) == 3 {
			files = append(files, f[2])
		}
	}
	lines = append(lines, fmt.Sprintf("diff vs %s: %d file(s); top dirs: %s", integration, len(files), coordTopDirs(files, 5)))
	lines = append(lines, "merge into "+integration+": "+coordMergeState(path, ref))
	tests := 0
	for _, f := range files {
		if coordTestPathRE.MatchString(f) {
			tests++
		}
	}
	lines = append(lines, fmt.Sprintf("tests touched: %d path(s)", tests))
	globs, found := coordScopeGlobs(coordTaskSpec(ctx, cfg, runID, p.TaskID))
	switch {
	case !found:
		lines = append(lines, "scope: no FILE SCOPE line")
	case len(globs) == 0:
		lines = append(lines, "scope: FILE SCOPE line has no parsable path globs")
	default:
		if oos := coordOutOfScope(files, globs); len(oos) == 0 {
			lines = append(lines, "scope: all changed files inside FILE SCOPE")
		} else {
			lines = append(lines, fmt.Sprintf("scope: %d file(s) OUTSIDE FILE SCOPE: %s", len(oos), coordTrunc(strings.Join(oos, ", "), 400)))
		}
	}
	return strings.Join(lines, "\n")
}

func coordShort(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func coordPushState(path, branch, head string) string {
	if branch == "" || branch == "HEAD" {
		return "unknown (detached HEAD, no branch name)"
	}
	out, _, err := coordGit(path, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	f := strings.Fields(out)
	switch {
	case len(f) == 0:
		return "NO (branch " + branch + " absent on origin)"
	case f[0] == head:
		return "yes (origin/" + branch + " == head)"
	default:
		return "NO (origin/" + branch + " is at " + coordShort(f[0]) + ", head is " + coordShort(head) + ")"
	}
}

func coordMergeState(path, ref string) string {
	out, code, err := coordGit(path, "merge-tree", "--write-tree", "--name-only", ref, "HEAD")
	if err != nil && code != 1 {
		return "unavailable (" + err.Error() + ")"
	}
	if code == 0 {
		return "clean"
	}
	var conflicts []string
	ls := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, l := range ls[min(1, len(ls)):] {
		if l == "" {
			break
		}
		conflicts = append(conflicts, l)
	}
	return fmt.Sprintf("CONFLICTS in %d path(s): %s", len(conflicts), coordTrunc(strings.Join(conflicts, ", "), 400))
}
