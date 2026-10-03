// cmd_nbus_resources.go — resource touches for bus.agent.activity.v1 and the
// bus.work.assignment.v1 emitter.
//
// Invariants:
//   - Refs are computed from the FULL tool input, before tool_summary
//     truncation. Only refs, intents and verbs leave this file; file contents,
//     command bodies and tool output never do.
//   - A path is only emitted when it is literal. Anything dynamic (variables,
//     globs in a Bash path, command substitution) yields no ref, never a guess.
//   - Repo resolution reads .git files directly (no git subprocess) and is
//     cached per directory for the life of the process.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	maxResourcesPerEvent = 12
	maxRefBytes          = 1024
	maxRepoWalkDepth     = 48
)

// resourceTouch is one element of the optional `resources` array.
type resourceTouch struct {
	Ref    string `json:"ref"`
	Intent string `json:"intent"`
}

// ---------------------------------------------------------------- repo cache

type repoInfo struct {
	root string // working-tree root (dir that holds .git)
	name string // logical repo name (main checkout's directory name)
	ok   bool
}

var (
	repoCacheMu sync.Mutex
	repoCache   = map[string]repoInfo{}
)

// resetRepoCache is for tests and benchmarks.
func resetRepoCache() {
	repoCacheMu.Lock()
	repoCache = map[string]repoInfo{}
	repoCacheMu.Unlock()
}

// resolveRepo walks up from dir to the nearest ancestor holding a `.git`
// entry. Every directory visited on the walk is cached, so a second lookup
// anywhere under the same root is a map hit.
func resolveRepo(dir string) repoInfo {
	repoCacheMu.Lock()
	defer repoCacheMu.Unlock()
	var visited [maxRepoWalkDepth]string
	n := 0
	var res repoInfo
	d := dir
	for depth := 0; depth < maxRepoWalkDepth && d != "" && d != "/" && d != "."; depth++ {
		if c, hit := repoCache[d]; hit {
			res = c
			break
		}
		visited[n] = d
		n++
		if fi, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			res = readRepoInfo(d, fi)
			break
		}
		d = filepath.Dir(d)
	}
	for i := 0; i < n; i++ {
		repoCache[visited[i]] = res
	}
	return res
}

// readRepoInfo names the repo at root. A `.git` directory names itself; a
// `.git` file (linked worktree) points at <main>/.git/worktrees/<n>, whose
// `commondir` leads back to <main>/.git, and <main>'s directory name is the
// repo name, so every worktree of one repo shares a name.
func readRepoInfo(root string, fi os.FileInfo) repoInfo {
	self := repoInfo{root: root, name: filepath.Base(root), ok: true}
	if fi.IsDir() {
		return self
	}
	raw, err := readSmallFile(filepath.Join(root, ".git"), 1024)
	if err != nil {
		return self
	}
	gitdir, found := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !found {
		return self
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	cd, err := readSmallFile(filepath.Join(gitdir, "commondir"), 1024)
	if err != nil {
		return self // submodule or plain gitfile: no shared common dir
	}
	common := strings.TrimSpace(string(cd))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	base := filepath.Base(common)
	switch {
	case base == ".git":
		self.name = filepath.Base(filepath.Dir(common))
	case strings.HasSuffix(base, ".git"):
		self.name = strings.TrimSuffix(base, ".git")
	}
	return self
}

func readSmallFile(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, max)
	n, err := f.Read(buf)
	if n == 0 && err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// refsForPath returns the physical `file:` ref and, when the path lies inside
// a git working tree, the logical `repo:<name>/<path-in-repo>` ref.
func refsForPath(abs string) (fileRef, repoRef string) {
	abs = filepath.Clean(abs)
	if !filepath.IsAbs(abs) || len(abs) > maxRefBytes {
		return "", ""
	}
	fileRef = "file:" + abs
	// Start at abs itself so a directory that is the repo root resolves; for a
	// file (or a not-yet-created one) the first lookup simply fails and the
	// walk continues upward.
	ri := resolveRepo(abs)
	if !ri.ok {
		return fileRef, ""
	}
	rel, err := filepath.Rel(ri.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") ||
		rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return fileRef, ""
	}
	if rel == "." {
		rel = ""
	}
	repoRef = "repo:" + ri.name + "/" + filepath.ToSlash(rel)
	if len(repoRef) > maxRefBytes {
		repoRef = ""
	}
	return fileRef, repoRef
}

// ---------------------------------------------------------------- extraction

type resourceSet struct{ items []resourceTouch }

func (s *resourceSet) add(ref, intent string) {
	if ref == "" || len(s.items) >= maxResourcesPerEvent {
		return
	}
	for _, it := range s.items {
		if it.Ref == ref && it.Intent == intent {
			return
		}
	}
	s.items = append(s.items, resourceTouch{Ref: ref, Intent: intent})
}

func (s *resourceSet) addPath(abs, intent string) {
	f, r := refsForPath(abs)
	s.add(f, intent)
	s.add(r, intent)
}

// activityInput is the typed subset of tool_input read for resource
// extraction. Unlisted fields (Write content, Edit strings) are skipped by the
// decoder without being materialised.
type activityInput struct {
	FilePath     string `json:"file_path"`
	Path         string `json:"path"`
	NotebookPath string `json:"notebook_path"`
	Pattern      string `json:"pattern"`
	Command      string `json:"command"`
}

func absJoin(cwd, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	return filepath.Join(cwd, p)
}

// extractResources derives the `resources` array from the full tool input.
// It returns nil when the tool touches nothing it can name.
func extractResources(toolName, cwd string, raw json.RawMessage) []resourceTouch {
	var set resourceSet
	switch toolName {
	case "Agent", "Task", "Workflow":
		set.add("cmd:"+strings.ToLower(toolName), "exec")
		return set.items
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "Read", "Grep", "Glob", "Bash":
	default:
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	var in activityInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	switch toolName {
	case "Write", "Edit", "MultiEdit":
		p := in.FilePath
		if p == "" {
			p = in.Path
		}
		set.addPath(absJoin(cwd, p), "write")
	case "NotebookEdit":
		p := in.NotebookPath
		if p == "" {
			p = in.FilePath
		}
		set.addPath(absJoin(cwd, p), "write")
	case "Read":
		p := in.FilePath
		if p == "" {
			p = in.Path
		}
		set.addPath(absJoin(cwd, p), "read")
	case "Grep":
		root := in.Path
		if root == "" {
			root = cwd
		}
		set.addPath(absJoin(cwd, root), "read")
	case "Glob":
		set.addPath(globRoot(cwd, in.Path, in.Pattern), "read")
	case "Bash":
		bashResources(&set, cwd, in.Command)
	}
	return set.items
}

// globRoot returns the deepest literal directory prefix of a glob pattern,
// anchored at base (or cwd). "src/**/*.go" -> <base>/src.
func globRoot(cwd, base, pattern string) string {
	if base == "" {
		base = cwd
	}
	root := absJoin(cwd, base)
	if root == "" && filepath.IsAbs(pattern) {
		root = "/"
	}
	if pattern == "" {
		return root
	}
	segs := strings.Split(pattern, "/")
	var lit []string
	for i, s := range segs {
		if strings.ContainsAny(s, "*?[{") {
			break
		}
		if i == len(segs)-1 && s != "" {
			// A fully literal pattern names a file or dir; keep it.
			lit = append(lit, s)
			break
		}
		lit = append(lit, s)
	}
	prefix := strings.Join(lit, "/")
	if filepath.IsAbs(pattern) {
		if prefix == "" {
			return "/"
		}
		return filepath.Clean(prefix)
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, prefix)
}

// ---------------------------------------------------------------------- bash

type shTok struct {
	s   string
	lit bool // no variable, substitution, glob or home expansion involved
}

// splitShell splits a command into segments of words on unquoted ; & | and
// newlines. It stops at the first heredoc operator, subshell or group so those
// bodies are never read as commands.
func splitShell(cmd string) [][]shTok {
	var segs [][]shTok
	var cur []shTok
	var word []byte
	inWord, lit := false, true
	flushWord := func() {
		if inWord {
			cur = append(cur, shTok{s: string(word), lit: lit})
		}
		word, inWord, lit = word[:0], false, true
	}
	flushSeg := func() {
		flushWord()
		if len(cur) > 0 {
			segs = append(segs, cur)
		}
		cur = nil
	}
	var q byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if q != 0 {
			if c == q {
				q = 0
				continue
			}
			if q == '"' && (c == '$' || c == '`') {
				lit = false
			}
			if q == '"' && c == '\\' && i+1 < len(cmd) {
				i++
				c = cmd[i]
			}
			word = append(word, c)
			continue
		}
		switch c {
		case '\'', '"':
			q, inWord = c, true
		case '\\':
			if i+1 < len(cmd) {
				i++
				word, inWord = append(word, cmd[i]), true
			}
		case ' ', '\t':
			flushWord()
		case '&':
			// `2>&1`, `<&0` and `&>file` are redirections, not separators.
			if (i > 0 && (cmd[i-1] == '>' || cmd[i-1] == '<')) || (i+1 < len(cmd) && cmd[i+1] == '>') {
				word, inWord = append(word, c), true
				continue
			}
			flushSeg()
		case '\n', ';', '|':
			flushSeg()
		case '<':
			if i+1 < len(cmd) && cmd[i+1] == '<' {
				flushSeg()
				return segs
			}
			word, inWord = append(word, c), true
		case '(', '{':
			if !inWord {
				// Subshell or group: everything after it is unparsed, not guessed.
				cur = nil
				return segs
			}
			lit = false
			word, inWord = append(word, c), true
		case '$', '`', '*', '?', '[':
			lit = false
			word, inWord = append(word, c), true
		case '~':
			if !inWord {
				lit = false
			}
			word, inWord = append(word, c), true
		default:
			word, inWord = append(word, c), true
		}
	}
	flushSeg()
	return segs
}

var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var verbWrappers = map[string]bool{"env": true, "time": true, "nohup": true, "nice": true, "command": true}

var verbChars = regexp.MustCompile(`^[A-Za-z0-9._+][A-Za-z0-9._+-]{0,31}$`)

// leadingVerb returns the executable name and the args after it.
func leadingVerb(seg []shTok) (string, []shTok) {
	i := 0
	for i < len(seg) {
		t := seg[i]
		if envAssign.MatchString(t.s) {
			if strings.Contains(t.s, "$(") || strings.Contains(t.s, "`") {
				// A substituted value such as `X=$(cmd -v y)` hides the real verb.
				return "", nil
			}
			i++
			continue
		}
		if t.lit && verbWrappers[t.s] {
			i++
			continue
		}
		break
	}
	if i >= len(seg) || !seg[i].lit {
		return "", nil
	}
	v := filepath.Base(seg[i].s)
	if !verbChars.MatchString(v) {
		return "", nil
	}
	return v, seg[i+1:]
}

// goTestValueFlags take a following argument, which must not be read as a
// package path.
var goTestValueFlags = map[string]bool{
	"-run": true, "-count": true, "-tags": true, "-timeout": true, "-p": true,
	"-parallel": true, "-bench": true, "-cpu": true, "-coverprofile": true,
	"-o": true, "-ldflags": true, "-gcflags": true, "-mod": true, "-skip": true,
	"-benchtime": true, "-covermode": true, "-coverpkg": true, "-exec": true,
	"-vet": true, "-shuffle": true, "-fuzz": true, "-overlay": true, "-pkgdir": true,
}

var pytestBoolFlags = map[string]bool{
	"-q": true, "-v": true, "-vv": true, "-vvv": true, "-x": true, "-s": true,
	"-l": true, "--lf": true, "--ff": true, "--sw": true, "--co": true,
	"--collect-only": true, "--no-header": true, "-ra": true, "-rA": true,
}

func bashResources(set *resourceSet, cwd, command string) {
	if command == "" {
		return
	}
	var verbs []string
	for _, seg := range splitShell(command) {
		verb, args := leadingVerb(seg)
		if verb == "" {
			continue
		}
		if verb == "cd" {
			if len(args) == 1 && args[0].lit && args[0].s != "-" {
				cwd = absJoin(cwd, args[0].s)
			} else {
				cwd = ""
			}
			continue
		}
		dup := false
		for _, v := range verbs {
			if v == verb {
				dup = true
			}
		}
		if !dup && len(verbs) < 3 {
			verbs = append(verbs, verb)
		}
		switch verb {
		case "git":
			if len(args) >= 2 && args[0].lit && args[0].s == "-C" && args[1].lit {
				set.addPath(absJoin(cwd, args[1].s), "exec")
			}
		case "make":
			for i := 0; i+1 < len(args); i++ {
				if args[i].lit && args[i].s == "-C" && args[i+1].lit {
					set.addPath(absJoin(cwd, args[i+1].s), "exec")
					break
				}
			}
		case "go":
			if len(args) >= 1 && args[0].lit && args[0].s == "test" {
				goTestPaths(set, cwd, args[1:])
			}
		case "pytest":
			pytestPaths(set, cwd, args)
		case "python", "python3":
			if len(args) >= 2 && args[0].lit && args[0].s == "-m" && args[1].lit && args[1].s == "pytest" {
				pytestPaths(set, cwd, args[2:])
			}
		}
	}
	for _, v := range verbs {
		set.add("cmd:"+v, "exec")
	}
}

func goTestPaths(set *resourceSet, cwd string, args []shTok) {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a.s, "-") {
			if !strings.Contains(a.s, "=") && goTestValueFlags["-"+strings.TrimLeft(a.s, "-")] {
				skipNext = true
			}
			continue
		}
		if !a.lit {
			continue
		}
		p := a.s
		if p != "." && !strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") && !filepath.IsAbs(p) {
			continue // import path or other non-path argument
		}
		p = strings.TrimSuffix(p, "/...")
		if p == "..." || strings.Contains(p, "...") {
			continue
		}
		set.addPath(absJoin(cwd, p), "exec")
	}
}

func pytestPaths(set *resourceSet, cwd string, args []shTok) {
	prevValueFlag := false
	for _, a := range args {
		if prevValueFlag {
			prevValueFlag = false
			continue
		}
		if strings.HasPrefix(a.s, "-") {
			if !strings.Contains(a.s, "=") && !pytestBoolFlags[a.s] {
				prevValueFlag = true
			}
			continue
		}
		if !a.lit {
			continue
		}
		p, _, _ := strings.Cut(a.s, "::")
		if !strings.Contains(p, "/") && !strings.HasSuffix(p, ".py") {
			continue
		}
		set.addPath(absJoin(cwd, p), "exec")
	}
}
