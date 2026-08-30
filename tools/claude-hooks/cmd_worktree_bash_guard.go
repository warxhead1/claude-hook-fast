package main

// cmdWorktreeBashGuard — GLOBAL PreToolUse guard for Bash: closes the shell
// escape hatches around worktree isolation, across ALL repos. Go port (v2)
// of ~/.claude/hooks/pre-tool-use/worktree-bash-guard.py.
//
// Companion to worktree-write-guard (Write/Edit/MultiEdit/NotebookEdit).
// This covers the Bash paths those cannot see:
//
//   A. git MUTATIONS in a primary checkout (add/commit/checkout/reset/merge/
//      rebase/apply/stash/clean/rm/mv/revert/switch/pull/push/am/cherry-pick)
//      are blocked for SUBAGENTS always, and for EVERYONE while a wave-freeze
//      marker is fresh. `git worktree ...`, read-only subcommands, read-only
//      stash variants (list/show), and --dry-run invocations pass.
//   B. file-writing shell commands (sed -i, tee, cp, mv, rm, touch, truncate,
//      dd, install, rsync, ln, shred, unlink) and output redirects whose
//      resolved target lands inside a primary checkout — blocked under the
//      same subagent-or-frozen condition. Allowlist matches the write guard:
//      .planning/ .claude/ .beads/ CLAUDE.md *.md.
//
// Resolution model (v2, 2026-07-17 — false-positive overhaul):
//   - EFFECTIVE CWD: `cd`/`pushd` segments update the cwd used to resolve
//     later segments; the payload cwd is only the starting point. Unknowable
//     cd targets (variables, `cd -`, popd) reset the cwd to unknown, and
//     relative paths then resolve to NOTHING (fail open) instead of being
//     mis-anchored onto the repo root.
//   - VARIABLES: NAME=value assignments earlier in the SAME command are
//     tracked and expanded in later tokens; $PWD tracks the effective cwd;
//     other names fall back to the hook's environment. Tokens still holding
//     `$` or command substitution after expansion are unresolvable ->
//     skipped, never joined onto the repo cwd.
//   - INLINE LIFT: a `wave-freeze off <root>` segment lifts the freeze for
//     later segments of the same command, so the lead can lift-and-integrate
//     in one Bash call. Subagent blocks are NOT liftable.
//   - EPHEMERAL ROOTS: primary checkouts under /tmp, /var/tmp, /dev/shm are
//     exempt (see worktree_common.go).
//   - EXISTENCE-AWARE TARGETS: writer args only count when plausible (target
//     must exist for sed -i / rm / shred / unlink; parent dir must exist for
//     creations), so sed scripts, dd operands, and other non-path tokens
//     stop resolving into the repo. cp/install/rsync/ln treat only the
//     DESTINATION as a write; fd redirects (>&2, 2>&1) are ignored;
//     `cmd | tee <target>` IS caught (writers are detected at pipe
//     boundaries, not just argv[0]).
//
// Exit 0 = allow; exit 2 = block. Kill switch: WORKTREE_GUARD_DISABLE=1.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

var gitMutating = map[string]bool{
	"add": true, "commit": true, "checkout": true, "restore": true,
	"reset": true, "merge": true, "rebase": true, "cherry-pick": true,
	"apply": true, "stash": true, "clean": true, "rm": true, "mv": true,
	"am": true, "revert": true, "switch": true, "pull": true, "push": true,
}

// Flags whose argument follows as a separate token (skipped when looking
// for the git subcommand).
var gitArgFlags = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true,
}

var writerCmds = map[string]bool{
	"tee": true, "cp": true, "mv": true, "rm": true, "touch": true,
	"truncate": true, "dd": true, "install": true, "rsync": true,
	"ln": true, "shred": true, "unlink": true, "sed": true,
}

// Tokens transparently stripped from the front of a segment before the
// command word is identified (shell keywords / wrappers).
var stripTokens = map[string]bool{
	"do": true, "done": true, "then": true, "else": true, "elif": true,
	"fi": true, "{": true, "}": true, "(": true, "!": true, "time": true,
	"command": true, "builtin": true, "exec": true, "nohup": true,
	"sudo": true, "doas": true, "export": true, "local": true,
	"declare": true, "typeset": true,
}

var (
	assignRe     = regexp.MustCompile(`^([A-Za-z_]\w*)=(.*)$`)
	fusedRedirRe = regexp.MustCompile(`^\d?>{1,2}(.+)$`)
)

type bashViolation struct {
	Why  string
	Seg  string
	Rule string // "subagent" or "wave-freeze" — same field style as write-guard's rule=
}

func cmdWorktreeBashGuard() {
	if wgDisabled() {
		return
	}
	input, ok := hookio.ReadHookInput()
	if !ok || input.ToolName != "Bash" {
		return
	}
	var ti struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil || ti.Command == "" {
		return
	}
	isAgent := hookio.IsSubagentTranscript(input.TranscriptPath)

	violations := bashGuardViolations(ti.Command, input.Cwd, isAgent)
	if len(violations) == 0 {
		return
	}

	logged := ti.Command
	if len(logged) > 200 {
		logged = logged[:200]
	}
	wgLog("bash-guard", "BLOCK",
		fmt.Sprintf("cmd=%q agent=%v cwd=%s rule=%s", logged, isAgent, input.Cwd, bashGuardRules(violations)))

	fmt.Fprint(os.Stderr, "WORKTREE BASH GUARD (global): blocked shell mutation of a "+
		"shared primary checkout.\n\n")
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "  Segment: %s\n  Reason:  %s\n\n", v.Seg, v.Why)
	}
	fmt.Fprint(os.Stderr,
		"Subagents never mutate the shared checkout of ANY repo, and during a\n"+
			"wave freeze nobody does. Work in your isolated worktree with\n"+
			"bare-relative paths, or in a scratch clone under /tmp (always exempt).\n"+
			"Read-only git, --dry-run, `git worktree`, and `git stash list/show`\n"+
			"always pass. If you have no worktree, STOP and escalate to the lead.\n"+
			"Lead: lift with `wave-freeze off <repo-root>` — inline in the same\n"+
			"command works: `wave-freeze off <root> && git merge ...`.\n")
	hookExit(2)
}

// bashGuardRules collapses one or more violations' Rule fields into a
// single deduped, order-preserving comma list for the log line — a bash
// command can trip both a git-mutation violation and a shell-writer
// violation in one call, and can trip subagent+wave-freeze together when
// a subagent runs during a freeze.
func bashGuardRules(violations []bashViolation) string {
	seen := map[string]bool{}
	var rules []string
	for _, v := range violations {
		if v.Rule == "" || seen[v.Rule] {
			continue
		}
		seen[v.Rule] = true
		rules = append(rules, v.Rule)
	}
	return strings.Join(rules, ",")
}

// bashGuardViolations is the pure core: model the command segment by
// segment and return every violation found. Fail open on anything
// unparseable.
func bashGuardViolations(command, startCwd string, isAgent bool) []bashViolation {
	command = stripHeredocs(command)
	cwd := startCwd
	vars := map[string]*string{}
	setPwd := func() {
		if cwd != "" {
			c := cwd
			vars["PWD"] = &c
		} else {
			vars["PWD"] = nil
		}
	}
	setPwd()

	lifted := map[string]bool{}
	liftAll := false
	var violations []bashViolation

	verdictFor := func(root, kind string) string {
		if kind != "primary" || root == "" {
			return ""
		}
		if isEphemeral(root) {
			return ""
		}
		if isAgent {
			return "subagent"
		}
		if liftAll || lifted[root] {
			return ""
		}
		if freezeActive(root) {
			return "wave-freeze"
		}
		return ""
	}
	checkWrite := func(target, seg string) {
		root, kind := repoOf(target)
		if kind == "primary" && allowlisted(target, root) {
			return
		}
		if why := verdictFor(root, kind); why != "" {
			violations = append(violations, bashViolation{
				Why: fmt.Sprintf("write to %s inside shared primary checkout %s (%s)",
					target, root, why),
				Seg:  seg,
				Rule: why,
			})
		}
	}

	for _, seg := range splitShellSegments(command) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		tokens, err := shellSplit(seg)
		if err != nil {
			continue
		}

		// Strip shell keywords / wrappers / leading assignments so the real
		// command word surfaces; record assignments for later expansion.
		for len(tokens) > 0 {
			t0 := tokens[0]
			if strings.HasPrefix(t0, "(") && len(t0) > 1 {
				tokens[0] = strings.TrimLeft(t0, "(")
				if tokens[0] == "" {
					tokens = tokens[1:]
				}
				continue
			}
			if stripTokens[t0] {
				tokens = tokens[1:]
				continue
			}
			if m := assignRe.FindStringSubmatch(t0); m != nil {
				if ev, ok := expandTok(m[2], vars); ok {
					v := expandUser(ev)
					vars[m[1]] = &v
				} else {
					vars[m[1]] = nil
				}
				tokens = tokens[1:]
				continue
			}
			break
		}
		if len(tokens) == 0 {
			continue
		}
		base0 := filepath.Base(tokens[0])

		// --- effective-cwd tracking ---
		if base0 == "cd" || base0 == "pushd" {
			if len(tokens) == 1 {
				home, err := os.UserHomeDir()
				if err == nil {
					cwd = home
				} else {
					cwd = ""
				}
			} else if t, ok := expandTok(tokens[1], vars); ok && t != "-" {
				t = expandUser(t)
				switch {
				case filepath.IsAbs(t):
					cwd = realpathBest(t)
				case cwd != "":
					cwd = realpathBest(filepath.Join(cwd, t))
				default:
					cwd = ""
				}
			} else {
				cwd = ""
			}
			setPwd()
			continue
		}
		if base0 == "popd" {
			cwd = ""
			setPwd()
			continue
		}

		// --- inline wave-freeze lift/re-arm ---
		if base0 == "wave-freeze" && len(tokens) >= 2 {
			arg := ""
			if len(tokens) > 2 {
				arg = tokens[2]
			}
			resolveArg := func() string {
				if arg == "" {
					return cwd
				}
				return resolvePath(arg, cwd, vars, false)
			}
			switch tokens[1] {
			case "off":
				if arg == "--all" {
					liftAll = true
				} else if p := resolveArg(); p != "" {
					lifted[primaryRootFor(p)] = true
				}
			case "on":
				if p := resolveArg(); p != "" {
					delete(lifted, primaryRootFor(p))
				}
				liftAll = false
			}
			continue
		}

		// --- A: git mutations in a primary checkout ---
		repoTok, hasRepoTok, sub, rest := gitParse(tokens)
		if gitMutating[sub] && !gitBenign(sub, rest) {
			repoDir := ""
			resolvable := true
			if hasRepoTok {
				if e, ok := expandTok(repoTok, vars); ok {
					e = expandUser(e)
					switch {
					case filepath.IsAbs(e):
						repoDir = e
					case cwd != "":
						repoDir = filepath.Join(cwd, e)
					default:
						resolvable = false
					}
				} else {
					resolvable = false // -C $VAR — cannot know the repo; fail open
				}
			} else {
				repoDir = cwd
			}
			if resolvable && repoDir != "" {
				root, kind := repoOf(repoDir)
				if why := verdictFor(root, kind); why != "" {
					violations = append(violations, bashViolation{
						Why: fmt.Sprintf("git %s targets shared primary checkout %s (%s)",
							sub, root, why),
						Seg:  seg,
						Rule: why,
					})
				}
			}
		}

		// --- B: shell writers (argv[0] or right after a pipe) ---
		for i, tok := range tokens {
			b := filepath.Base(tok)
			if !writerCmds[b] {
				continue
			}
			if i > 0 && tokens[i-1] != "|" {
				continue
			}
			var args []string
			for _, t := range tokens[i+1:] {
				if t == "|" {
					break
				}
				args = append(args, t)
			}
			for _, target := range writerTargets(b, args, cwd, vars) {
				checkWrite(target, seg)
			}
		}

		// --- B: output redirects ---
		for _, target := range redirectTargets(tokens, cwd, vars) {
			checkWrite(target, seg)
		}
	}
	return violations
}

// gitParse finds a `git` invocation in the segment and returns its -C
// argument (if any), the subcommand, and the tokens after the subcommand.
func gitParse(tokens []string) (repoTok string, hasRepoTok bool, sub string, rest []string) {
	for i, tok := range tokens {
		if filepath.Base(tok) != "git" {
			continue
		}
		j := i + 1
		for j < len(tokens) {
			t := tokens[j]
			if gitArgFlags[t] {
				if t == "-C" && j+1 < len(tokens) {
					repoTok = tokens[j+1]
					hasRepoTok = true
				}
				j += 2
				continue
			}
			if strings.HasPrefix(t, "--git-dir=") || strings.HasPrefix(t, "--work-tree=") {
				j++
				continue
			}
			if strings.HasPrefix(t, "-") {
				j++
				continue
			}
			return repoTok, hasRepoTok, t, tokens[j+1:]
		}
		return repoTok, hasRepoTok, "", nil
	}
	return "", false, "", nil
}

// gitBenign: read-only variants of otherwise-mutating verbs.
func gitBenign(sub string, rest []string) bool {
	for _, t := range rest {
		if t == "--dry-run" {
			return true
		}
	}
	if sub == "stash" {
		for _, t := range rest {
			if t == "|" {
				break // bare `git stash | ...` — default push mutates
			}
			if !strings.HasPrefix(t, "-") {
				return t == "list" || t == "show"
			}
		}
	}
	return false
}

// writerTargets: written-to paths for a writer command, honoring per-command
// semantics (sources of cp are NOT writes; sed without -i writes nothing;
// dd writes only of=...).
func writerTargets(base string, args []string, cwd string, vars map[string]*string) []string {
	var out []string
	add := func(tok string, mustExist bool) {
		if r := resolvePath(tok, cwd, vars, mustExist); r != "" {
			out = append(out, r)
		}
	}
	var positional []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
		}
	}
	switch base {
	case "sed":
		inPlace := false
		for _, a := range args {
			if a == "-i" || (strings.HasPrefix(a, "-i") && len(a) > 2) ||
				a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
				inPlace = true
				break
			}
		}
		if !inPlace {
			return nil
		}
		// sed -i edits EXISTING files; script/expression tokens fail the
		// existence check and drop out naturally.
		for _, a := range positional {
			add(a, true)
		}
	case "rm", "shred", "unlink":
		for _, a := range positional {
			add(a, true)
		}
	case "cp", "install", "rsync", "ln":
		tIdx := -1
		for i, a := range args {
			if a == "-t" {
				tIdx = i
				break
			}
		}
		if tIdx >= 0 && tIdx+1 < len(args) {
			add(args[tIdx+1], false)
		} else if len(positional) > 0 {
			add(positional[len(positional)-1], false)
		}
	case "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=") {
				add(strings.TrimPrefix(a, "of="), false)
			}
		}
	default: // tee, mv, touch, truncate
		for _, a := range positional {
			add(a, false)
		}
	}
	return out
}

// heredocOp is a `<<[-]TAG` / `<<[-]'TAG'` / `<<[-]"TAG"` operator found
// outside quotes, recording where its tag ends (idx) so the body that
// follows on later lines can be located.
type heredocOp struct {
	idx  int // index in command right after the tag (and its closing quote)
	tag  string
	dash bool // <<- : terminator line may have leading tabs stripped
}

// heredocOps quote-aware-scans command for heredoc redirect operators
// (`<<TAG`, `<<-TAG`, `<<'TAG'`, `<<"TAG"`), skipping here-strings (`<<<`)
// and anything inside single/double quotes. Returned in source order.
func heredocOps(command string) []heredocOp {
	var ops []heredocOp
	inSingle, inDouble := false, false
	i, n := 0, len(command)
	for i < n {
		c := command[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
			i++
		case inDouble:
			if c == '\\' && i+1 < n {
				i += 2
				continue
			}
			if c == '"' {
				inDouble = false
			}
			i++
		case c == '\\' && i+1 < n:
			i += 2
		case c == '\'':
			inSingle = true
			i++
		case c == '"':
			inDouble = true
			i++
		case c == '<' && i+1 < n && command[i+1] == '<' && (i+2 >= n || command[i+2] != '<'):
			j := i + 2
			dash := false
			if j < n && command[j] == '-' {
				dash = true
				j++
			}
			for j < n && (command[j] == ' ' || command[j] == '\t') {
				j++
			}
			var tag string
			if j < n && (command[j] == '\'' || command[j] == '"') {
				q := command[j]
				j++
				ts := j
				for j < n && command[j] != q {
					j++
				}
				tag = command[ts:j]
				if j < n {
					j++ // closing quote
				}
			} else {
				ts := j
				for j < n && !strings.ContainsRune(" \t\n;&|<>", rune(command[j])) {
					j++
				}
				tag = command[ts:j]
			}
			if tag != "" {
				ops = append(ops, heredocOp{idx: j, tag: tag, dash: dash})
			}
			i = j
		default:
			i++
		}
	}
	return ops
}

// stripHeredocs removes heredoc BODIES from command (replacing them with
// nothing) while leaving the operator line itself — including anything
// else on that same line, such as a trailing ` > file` redirect — intact.
// Without this, splitShellSegments/shellSplit treat heredoc body lines as
// ordinary shell text: a read-only SQL query containing `WHERE trade_date
// >= '2026-08-10'` or `(col->>'Qty')::double precision > 0` piped through a
// heredoc was parsed as an output redirect to a file literally named `=` or
// `0`, false-blocking a read-only command (measured 2026-08-30, 58 blocked
// calls in one session). A genuine redirect placed OUTSIDE the heredoc body
// (`cat <<EOF > file`) stays on the copied operator line and is still seen.
func stripHeredocs(command string) string {
	ops := heredocOps(command)
	if len(ops) == 0 {
		return command
	}
	var out strings.Builder
	cursor := 0
	n := len(command)
	idx := 0
	for idx < len(ops) {
		first := ops[idx]
		lineEnd := n
		if nl := strings.IndexByte(command[first.idx:], '\n'); nl != -1 {
			lineEnd = first.idx + nl + 1
		}
		// Multiple heredocs can share one line (`cmd <<A <<B`); their
		// bodies are consumed in order immediately after that shared line.
		group := []heredocOp{first}
		idx++
		for idx < len(ops) && ops[idx].idx < lineEnd {
			group = append(group, ops[idx])
			idx++
		}
		if lineEnd > cursor {
			out.WriteString(command[cursor:lineEnd])
		}
		cursor = lineEnd
		for _, op := range group {
			cursor = skipHeredocBody(command, cursor, op.tag, op.dash)
		}
	}
	if cursor < n {
		out.WriteString(command[cursor:])
	}
	return out.String()
}

// skipHeredocBody returns the index just past the heredoc body starting at
// pos: every line up to and including the one that equals tag exactly
// (leading tabs stripped first when dash is set, per `<<-`). An unterminated
// heredoc consumes to the end of the command (fail open elsewhere — there's
// nothing left to mis-parse as a redirect).
func skipHeredocBody(command string, pos int, tag string, dash bool) int {
	n := len(command)
	for pos < n {
		nl := strings.IndexByte(command[pos:], '\n')
		var line string
		var next int
		if nl == -1 {
			line, next = command[pos:], n
		} else {
			line, next = command[pos:pos+nl], pos+nl+1
		}
		check := line
		if dash {
			check = strings.TrimLeft(line, "\t")
		}
		if check == tag {
			return next
		}
		if nl == -1 {
			return n
		}
		pos = next
	}
	return pos
}

// redirectTargets resolves output-redirect targets; fd duplication (>&2,
// 2>&1) is ignored.
func redirectTargets(tokens []string, cwd string, vars map[string]*string) []string {
	var out []string
	for i, tok := range tokens {
		target := ""
		switch {
		case tok == ">" || tok == ">>" || tok == "&>" || tok == "&>>":
			if i+1 < len(tokens) {
				target = tokens[i+1]
			}
		case strings.HasPrefix(tok, "&>>"):
			target = tok[3:]
		case strings.HasPrefix(tok, "&>"):
			target = tok[2:]
		default:
			if m := fusedRedirRe.FindStringSubmatch(tok); m != nil {
				target = m[1]
			}
		}
		if target == "" || strings.HasPrefix(target, "&") {
			continue
		}
		if r := resolvePath(target, cwd, vars, false); r != "" {
			out = append(out, r)
		}
	}
	return out
}
