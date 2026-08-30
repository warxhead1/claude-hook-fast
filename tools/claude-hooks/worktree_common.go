package main

// Shared plumbing for the worktree-isolation guard family
// (worktree-bash-guard, worktree-write-guard, dispatch-freeze-guard) —
// ports of the ~/.claude/hooks/pre-tool-use/*.py one-offs into this binary.
//
// Detection is STRUCTURAL, not a repo list:
//   - a directory whose .git is a DIRECTORY -> primary (shared) checkout
//   - a directory whose .git is a FILE      -> git worktree (isolated)
//
// Primary checkouts under /tmp, /var/tmp, /dev/shm are EPHEMERAL (scratch
// clones, merge trains, session scratchpads) and exempt from all guard
// rules — isolation protects shared long-lived checkouts, not throwaway
// ones. Tests restore strictness with WORKTREE_GUARD_STRICT_TMP=1.
//
// Kill switch for the whole family: WORKTREE_GUARD_DISABLE=1.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const wgTTL = 2 * time.Hour

var wgAllowRelPrefixes = []string{".planning/", ".claude/", ".beads/"}
var wgEphemeralPrefixes = ephemeralPrefixes()

// ephemeralPrefixes returns the roots under which a checkout counts as a
// scratch/ephemeral tree. $TMPDIR is included because this box redirects it
// off tmpfs (data2-disk ruling): scratch clones made with mktemp land there,
// and test sandboxes (t.TempDir) must stay exempt wherever TMPDIR points.
func ephemeralPrefixes() []string {
	pre := []string{"/tmp/", "/var/tmp/", "/dev/shm/"}
	if td := strings.TrimSuffix(os.TempDir(), "/"); td != "" && td != "/tmp" {
		pre = append(pre, td+"/")
	}
	return pre
}

func wgDisabled() bool {
	return os.Getenv("WORKTREE_GUARD_DISABLE") == "1"
}

func wgFreezeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.claude-wave-freeze"
	}
	return filepath.Join(home, ".claude", "wave-freeze")
}

// wgLog appends to the shared worktree-guard.log (same file and line format
// as the retired Python guards, so existing log tooling keeps working).
func wgLog(component, verdict, detail string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".claude", "hooks", "log")
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "worktree-guard.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	fmt.Fprintf(f, "%s %s %s %s\n",
		time.Now().Format("2006-01-02T15:04:05"), component, verdict, detail)
}

// realpathBest mirrors Python's os.path.realpath: best-effort symlink
// resolution that still returns a cleaned path when components don't exist.
func realpathBest(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	abs = filepath.Clean(abs)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir := filepath.Dir(abs)
	if dir == abs {
		return abs
	}
	return filepath.Join(realpathBest(dir), filepath.Base(abs))
}

// repoOf walks up from path to the nearest .git entry.
// kind is "primary", "worktree", or "" (not inside a repo).
// A .git DIRECTORY only counts as a repo when it contains HEAD (git's own
// minimum) — a stray empty .git dir (observed: /tmp/.git, 2026-07-21) must
// not turn its parent into a phantom "primary checkout".
func repoOf(path string) (root, kind string) {
	p := realpathBest(path)
	for {
		gp := filepath.Join(p, ".git")
		fi, err := os.Stat(gp)
		if err == nil {
			if fi.IsDir() {
				if _, err := os.Stat(filepath.Join(gp, "HEAD")); err == nil {
					return p, "primary"
				}
				// invalid/empty .git dir: not a repo boundary, keep walking
			} else {
				return p, "worktree"
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", ""
		}
		p = parent
	}
}

// primaryRootFor resolves a path to the primary-checkout root it denotes,
// following a worktree's gitdir pointer back to its primary (mirrors
// `wave-freeze off` resolving via --git-common-dir). Falls back to the
// realpath itself when the pointer can't be followed.
func primaryRootFor(path string) string {
	p := realpathBest(path)
	root, kind := repoOf(p)
	switch kind {
	case "primary":
		return root
	case "worktree":
		if pr, ok := worktreePrimary(root); ok {
			return pr
		}
	}
	return p
}

// worktreePrimary parses a worktree root's .git file
// ("gitdir: <primary>/.git/worktrees/<name>") back to the primary root.
func worktreePrimary(worktreeRoot string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(worktreeRoot, ".git"))
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if !strings.HasPrefix(line, "gitdir:") {
		return "", false
	}
	gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	k := strings.Index(gd, "/.git/worktrees/")
	if k <= 0 {
		return "", false
	}
	return realpathBest(gd[:k]), true
}

func isEphemeral(root string) bool {
	if os.Getenv("WORKTREE_GUARD_STRICT_TMP") == "1" {
		return false
	}
	for _, pre := range wgEphemeralPrefixes {
		if strings.HasPrefix(root, pre) || root == strings.TrimSuffix(pre, "/") {
			return true
		}
	}
	return false
}

func freezeMarkerPath(root string) string {
	return filepath.Join(wgFreezeDir(),
		strings.ReplaceAll(strings.Trim(root, "/"), "/", "__"))
}

func freezeActive(root string) bool {
	fi, err := os.Stat(freezeMarkerPath(root))
	if err != nil {
		return false
	}
	if time.Since(fi.ModTime()) > wgTTL {
		_ = os.Remove(freezeMarkerPath(root))
		return false
	}
	return true
}

// allowlisted: paths every guard lets through inside a primary checkout —
// planning/config/bead metadata and hand-authored docs.
func allowlisted(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "CLAUDE.md" {
		return true
	}
	for _, pre := range wgAllowRelPrefixes {
		if strings.HasPrefix(rel, pre) {
			return true
		}
	}
	return strings.HasSuffix(target, ".md")
}

// --- shell-command modeling helpers (bash guard) ---

// shellSplit tokenizes like Python's shlex.split: whitespace-separated words
// with POSIX quote handling; quotes are stripped, redirect characters are
// ordinary characters. Errors on unbalanced quotes (caller skips the
// segment — fail open).
func shellSplit(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inTok := false
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '\'':
			inTok = true
			j := i + 1
			for j < n && s[j] != '\'' {
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unbalanced single quote")
			}
			cur.WriteString(s[i+1 : j])
			i = j + 1
		case c == '"':
			inTok = true
			i++
			for i < n && s[i] != '"' {
				if s[i] == '\\' && i+1 < n &&
					(s[i+1] == '"' || s[i+1] == '\\' || s[i+1] == '$' || s[i+1] == '`') {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unbalanced double quote")
			}
			i++
		case c == '\\':
			if i+1 < n {
				cur.WriteByte(s[i+1])
				inTok = true
			}
			i += 2
		case c == ' ' || c == '\t' || c == '\r':
			if inTok {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inTok = false
			}
			i++
		default:
			cur.WriteByte(c)
			inTok = true
			i++
		}
	}
	if inTok {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

var wgVarRe = regexp.MustCompile(`\$\{(\w+)\}|\$(\w+)`)

// expandTok expands $NAME/${NAME} from in-command assignments (nil value =
// known-unknown), falling back to the hook's environment. Returns ok=false
// when the token cannot be FULLY resolved (unknown variable, command
// substitution, backticks) — callers must then SKIP it, never guess.
func expandTok(tok string, vars map[string]*string) (string, bool) {
	if strings.Contains(tok, "`") || strings.Contains(tok, "$(") {
		return "", false
	}
	ok := true
	out := wgVarRe.ReplaceAllStringFunc(tok, func(m string) string {
		sub := wgVarRe.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[2]
		}
		if v, present := vars[name]; present {
			if v == nil {
				ok = false
				return m
			}
			return *v
		}
		if v, present := os.LookupEnv(name); present {
			return v
		}
		ok = false
		return m
	})
	if !ok || strings.Contains(out, "$") {
		return "", false
	}
	return out, true
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// resolvePath resolves a candidate path token against the effective cwd, or
// returns "" when it is unresolvable or implausible as a write target
// (nonexistent parent dir; nonexistent file when mustExist).
func resolvePath(tok, cwd string, vars map[string]*string, mustExist bool) string {
	if tok == "" || strings.HasPrefix(tok, "-") {
		return ""
	}
	t, ok := expandTok(tok, vars)
	if !ok {
		return ""
	}
	t = expandUser(t)
	if !filepath.IsAbs(t) {
		if cwd == "" {
			return ""
		}
		t = filepath.Join(cwd, t)
	}
	t = realpathBest(t)
	if mustExist {
		if _, err := os.Lstat(t); err != nil {
			return ""
		}
		return t
	}
	if fi, err := os.Stat(filepath.Dir(t)); err == nil && fi.IsDir() {
		return t
	}
	return ""
}

// splitShellSegments splits a shell command into segments on the unquoted
// separators `&&`, `||`, `;`, and newline — the same delimiter set the old
// segSplitRe regex used, minus its defect: the regex split the RAW string,
// blind to quoting, so a multi-line single- or double-quoted argument (SQL
// piped to psql was the measured case, 2026-08-05: `echo "... WHERE
// consecutive_failures > 0" | db-readonly.sh`) was chopped mid-string and a
// bare `> 0` inside the quotes parsed as an output redirect to a file named
// "0", falsely blocking a read-only command. Quote state (single quotes,
// double quotes with backslash escapes, and backslash escapes outside
// quotes) is tracked so separators inside quotes never split. Unbalanced
// quotes fall back to splitting the remainder as the regex would have —
// the guard family already tolerates unparseable segments by skipping them.
func splitShellSegments(command string) []string {
	var segs []string
	var cur strings.Builder
	flush := func() {
		segs = append(segs, cur.String())
		cur.Reset()
	}
	i, n := 0, len(command)
	inSingle, inDouble := false, false
	for i < n {
		c := command[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
			cur.WriteByte(c)
			i++
		case inDouble:
			if c == '\\' && i+1 < n {
				cur.WriteByte(c)
				cur.WriteByte(command[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inDouble = false
			}
			cur.WriteByte(c)
			i++
		case c == '\\' && i+1 < n:
			cur.WriteByte(c)
			cur.WriteByte(command[i+1])
			i += 2
		case c == '\'':
			inSingle = true
			cur.WriteByte(c)
			i++
		case c == '"':
			inDouble = true
			cur.WriteByte(c)
			i++
		case c == ';' || c == '\n':
			flush()
			i++
		case c == '&' && i+1 < n && command[i+1] == '&':
			flush()
			i += 2
		case c == '|' && i+1 < n && command[i+1] == '|':
			flush()
			i += 2
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return segs
}
