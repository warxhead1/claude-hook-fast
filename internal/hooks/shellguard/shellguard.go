// Package shellguard is the single-source port of the Claude-side Bash
// PreToolUse guards (~/.claude/hook-rules.json, this repo's
// configs/global-hook-rules.json, and ~/.claude/hooks/pre-tool-use/
// recursive-delete-guard.py) for consumption by codex-hook.
//
// Claude Code enforces these through a data-driven rules engine
// (tools/claude-hooks/cmd_rules.go) that reads a project-tiered JSON config
// with counters, pipelines, and Claude-only builtins (agent_no_isolation,
// duplicate_binding_spec, ...). Most of that machinery does not apply to
// Codex (no per-project config walk, no Agent/Task tool_input shape), so
// this package extracts only the STATIC, universally-applicable command
// guards — the ones that block a destructive shell invocation outright,
// regardless of project — into a single embedded asset (guardrails.json)
// that both codex-hook and, in principle, claude-hook-fast could consume.
//
// guardrails.json's regex patterns and messages mirror the live
// ~/.claude/hook-rules.json rule IDs of the same name; keep them in sync by
// hand when either side changes (there is no automated cross-check because
// the live config lives outside this repo, per-machine).
//
// The one guard NOT expressible as a single regex — recursive rm/find
// outside allow-listed scratch roots — is ported procedurally from
// recursive-delete-guard.py's allow-root logic (InspectRecursiveDelete).
//
// Fail-open throughout: a malformed command segment, an unknown quoting
// shape, or a JSON parse failure never blocks — it is skipped or (for the
// embedded asset) causes Inspect to report no violations.
package shellguard

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

//go:embed guardrails.json
var assets embed.FS

// Violation is a single denied command, carrying enough context to write a
// PreToolUse deny reason and a diagnostic event.
type Violation struct {
	RuleID  string
	Message string
}

type guardrailsFile struct {
	Version int           `json:"version"`
	Rules   []guardrailRC `json:"rules"`
}

type guardrailRC struct {
	ID           string `json:"id"`
	CommandRegex string `json:"command_regex"`
	Message      string `json:"message"`
}

type compiledRule struct {
	id      string
	re      *regexp.Regexp
	message string
}

var (
	loadOnce sync.Once
	rules    []compiledRule
)

// loadRules parses the embedded guardrails.json once and compiles its
// regexes. A malformed asset (should never happen — it ships with the
// binary) yields an empty rule set rather than a panic, consistent with
// this package's fail-open contract.
func loadRules() []compiledRule {
	loadOnce.Do(func() {
		data, err := assets.ReadFile("guardrails.json")
		if err != nil {
			return
		}
		var gf guardrailsFile
		if err := json.Unmarshal(data, &gf); err != nil {
			return
		}
		for _, r := range gf.Rules {
			if r.ID == "" || r.CommandRegex == "" {
				continue
			}
			re, err := regexp.Compile(r.CommandRegex)
			if err != nil {
				continue
			}
			rules = append(rules, compiledRule{id: r.ID, re: re, message: r.Message})
		}
	})
	return rules
}

// Inspect runs every static command-regex guard plus the recursive-delete
// guard against a single shell command string, returning the first
// violation (regex guards first, in guardrails.json order, then the
// recursive-delete guard) or nil when the command is clean.
//
// cwd resolves relative rm/find operands, matching recursive-delete-guard.py.
func Inspect(command, cwd string) *Violation {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	for _, r := range loadRules() {
		if r.re.MatchString(command) {
			return &Violation{RuleID: r.id, Message: r.message}
		}
	}
	return InspectRecursiveDelete(command, cwd)
}

// ── recursive-delete guard (ported from recursive-delete-guard.py) ─────────

// rdgLeaders are shell words that precede the real command and must be
// stripped before name-matching (until/while/if/.../sudo/env), mirroring
// recursive-delete-guard.py's LEADERS set.
var rdgLeaders = map[string]bool{
	"until": true, "while": true, "if": true, "then": true, "do": true,
	"else": true, "elif": true, "!": true, "time": true, "nohup": true,
	"command": true, "builtin": true, "exec": true, "sudo": true, "env": true,
}

var rdgAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// rdgAllowRoots mirrors ALLOW_ROOTS in recursive-delete-guard.py.
var rdgAllowRoots = []string{
	"/home/eric/data2/claude-tmp",
	"/home/eric/data2/tmp",
	"/home/eric/.cache",
	"/tmp",
}

// rdgAllowPattern mirrors ALLOW_PATTERNS.
var rdgAllowPattern = regexp.MustCompile(`^/home/eric/data2/orca/[^/]+/_evidence(/|$)`)

func rdgAllowed(path, cwd string) bool {
	p := path
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	roots := append([]string{}, rdgAllowRoots...)
	if extra := os.Getenv("CLAUDE_CODE_TMPDIR"); extra != "" {
		roots = append(roots, extra)
	}
	for _, r := range roots {
		r = strings.TrimSuffix(r, "/")
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return rdgAllowPattern.MatchString(p)
}

func rdgRmRecursive(tokens []string) bool {
	for _, t := range tokens[1:] {
		if t == "--" {
			break
		}
		if t == "--recursive" {
			return true
		}
		if len(t) > 1 && t[0] == '-' && t[1] != '-' && (strings.ContainsRune(t[1:], 'r') || strings.ContainsRune(t[1:], 'R')) {
			return true
		}
	}
	return false
}

func rdgRmOperands(tokens []string) []string {
	var ops []string
	for _, t := range tokens[1:] {
		if strings.HasPrefix(t, "-") && t != "-" {
			continue
		}
		ops = append(ops, t)
	}
	return ops
}

func rdgFindDeletes(tokens []string) bool {
	for _, t := range tokens {
		if t == "-delete" {
			return true
		}
	}
	for i, t := range tokens {
		if (t == "-exec" || t == "-execdir" || t == "-ok" || t == "-okdir") && i+1 < len(tokens) {
			base := filepath.Base(tokens[i+1])
			if base == "rm" || base == "rmdir" || base == "unlink" {
				return true
			}
		}
	}
	return false
}

func rdgFindOperands(tokens []string) []string {
	var ops []string
	for _, t := range tokens[1:] {
		if strings.HasPrefix(t, "-") || t == "(" || t == ")" || t == "!" || t == "," {
			break
		}
		ops = append(ops, t)
	}
	if len(ops) == 0 {
		return []string{"."}
	}
	return ops
}

func rdgGitClean(tokens []string) bool {
	if len(tokens) < 2 {
		return false
	}
	i := 1
	for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
		if tokens[i] == "-C" || tokens[i] == "-c" {
			i += 2
		} else {
			i++
		}
	}
	if i >= len(tokens) || tokens[i] != "clean" {
		return false
	}
	for _, t := range tokens[i+1:] {
		if t == "--force" {
			return true
		}
		if len(t) > 1 && t[0] == '-' && t[1] != '-' {
			for _, c := range t[1:] {
				if c == 'f' || c == 'd' || c == 'x' || c == 'X' {
					return true
				}
			}
		}
	}
	return false
}

// segmentSplit mirrors recursive-delete-guard.py's SPLIT regex: split on
// ||, &&, |, ;, or newline (quote-blind, matching the Python original —
// good enough for the destructive-command shapes this guard targets).
var rdgSegmentSplit = regexp.MustCompile(`\|\||&&|[|;&\n]`)

// InspectRecursiveDelete blocks `rm -r`/`-R`/`--recursive`, `find -delete`
// or `find -exec rm ...`, and `git clean -f/-d/-x/-X` unless every path
// operand resolves under an allow-listed scratch root. A `rm -rf` with no
// literal path operand (variables only) is blocked — unverifiable.
func InspectRecursiveDelete(command, cwd string) *Violation {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	for _, segment := range rdgSegmentSplit.Split(command, -1) {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		tokens, err := shellSplit(segment)
		if err != nil || len(tokens) == 0 {
			continue
		}
		for len(tokens) > 0 && (rdgLeaders[tokens[0]] || rdgAssignRe.MatchString(tokens[0])) {
			tokens = tokens[1:]
		}
		if len(tokens) == 0 {
			continue
		}
		name := filepath.Base(tokens[0])
		switch {
		case name == "rm" && rdgRmRecursive(tokens):
			ops := rdgRmOperands(tokens)
			var unsafe []string
			for _, o := range ops {
				if strings.HasPrefix(o, "$") || !rdgAllowed(o, cwd) {
					unsafe = append(unsafe, o)
				}
			}
			if len(ops) == 0 || len(unsafe) > 0 {
				return &Violation{RuleID: "recursive-delete-rm", Message: rdgBlockMessage("rm -r", unsafeOrPlaceholder(unsafe))}
			}
		case name == "find" && rdgFindDeletes(tokens):
			ops := rdgFindOperands(tokens)
			var unsafe []string
			for _, o := range ops {
				if !rdgAllowed(o, cwd) {
					unsafe = append(unsafe, o)
				}
			}
			if len(unsafe) > 0 {
				return &Violation{RuleID: "recursive-delete-find", Message: rdgBlockMessage("find -delete/-exec rm", unsafe)}
			}
		case name == "git" && rdgGitClean(tokens):
			return &Violation{RuleID: "recursive-delete-git-clean", Message: rdgBlockMessage("git clean -f", []string{cwd})}
		}
	}
	return nil
}

func unsafeOrPlaceholder(unsafe []string) []string {
	if len(unsafe) == 0 {
		return []string{"<no literal path>"}
	}
	return unsafe
}

func rdgBlockMessage(kind string, paths []string) string {
	return "BLOCKED: recursive delete (" + kind + ") on " + strings.Join(paths, ", ") + ".\n\n" +
		"Recursive deletes inside a checkout or worktree are not available to agents.\n" +
		"An orphan directory may hold the only copy of someone's work; a move keeps\n" +
		"it, a delete does not.\n\n" +
		"Do this instead:\n" +
		"  mv <path> /home/eric/data2/orca/<project>/_evidence/<date>/<lane>/scratch/retired/\n" +
		"  git restore <tracked-file>      # to undo your own edit to a tracked file\n\n" +
		"Deletes under claude-tmp, ~/.cache, /tmp, and */_evidence/ pass unchanged."
}

// shellSplit tokenizes like Python's shlex.split: whitespace-separated
// words with POSIX quote handling; quotes are stripped. Ported from
// tools/claude-hooks/worktree_common.go (package main there cannot be
// imported from cmd/codex-hook, hence the duplication — kept intentionally
// tiny and dependency-free so the two copies are easy to eyeball together).
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
				return nil, errUnbalancedQuote
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
				return nil, errUnbalancedQuote
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

type shellSplitError string

func (e shellSplitError) Error() string { return string(e) }

const errUnbalancedQuote = shellSplitError("unbalanced quote")
