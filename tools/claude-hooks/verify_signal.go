package main

// verify_signal.go: verify classification and outcome for bus.agent.activity.v1.
//
// The Bash tool_summary went verb-only on 2026-10-04 so no argument text can
// reach the bus. That also hid every `cargo test` from the reflex verification
// detectors, and no failed call ever reached the bus (PostToolUse fires only on
// success). These helpers read the FULL command and output inside the hook
// and emit only enums from fixed tables: the command, its arguments and the
// tool output never leave this process.

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

// Verify classes, strongest last. A compound `cargo build && cargo test`
// reads as test.
var verifyRank = map[string]int{"lint": 1, "typecheck": 2, "build": 3, "test": 4}

// classifyVerify returns the verify class of a Bash command and a canonical
// phrase for it ("cargo test", "npm run lint"). Both come from fixed tables.
// Empty when no segment is a verification.
func classifyVerify(cmd string) (class, phrase string) {
	for _, seg := range splitShell(cmd) {
		c, p := classifyVerifySegment(seg)
		if verifyRank[c] > verifyRank[class] {
			class, phrase = c, p
		}
	}
	return class, phrase
}

var (
	// Valued flags per tool, so a flag's value is not read as the subcommand.
	cargoValued  = map[string]bool{"-C": true, "--config": true, "-Z": true, "--manifest-path": true, "--color": true}
	goValued     = map[string]bool{"-C": true}
	makeValued   = map[string]bool{"-C": true, "-f": true, "-I": true, "-o": true, "-W": true, "--directory": true, "--file": true, "-j": true, "-l": true}
	pythonValued = map[string]bool{"-W": true, "-X": true, "-c": true}
	npmValued    = map[string]bool{"--prefix": true, "-C": true, "--dir": true, "--filter": true, "-F": true, "-w": true, "--workspace": true, "--cwd": true}
	gradleValued = map[string]bool{"-p": true, "-b": true, "-c": true, "--project-dir": true}

	cargoSub = map[string]string{"test": "test", "nextest": "test", "build": "build", "check": "build", "clippy": "lint"}
	goSub    = map[string]string{"test": "test", "build": "build", "vet": "lint"}
	makeSub  = map[string]string{"test": "test", "tests": "test", "check": "test", "verify": "test", "ci": "test", "build": "build", "lint": "lint", "typecheck": "typecheck"}
	mvnSub   = map[string]string{"test": "test", "verify": "test", "check": "test", "build": "build", "compile": "build", "package": "build", "assemble": "build"}
	dotSub   = map[string]string{"test": "test", "build": "build"}
	pyMods   = map[string]string{"pytest": "test", "unittest": "test", "mypy": "typecheck", "pyright": "typecheck", "ruff": "lint", "flake8": "lint", "pylint": "lint", "compileall": "build", "py_compile": "build"}
	bareVerb = map[string]string{
		"pytest": "test", "py.test": "test", "vitest": "test", "jest": "test", "mocha": "test",
		"playwright": "test", "ctest": "test", "bats": "test", "nextest": "test",
		"tsc": "typecheck", "vue-tsc": "typecheck", "mypy": "typecheck", "pyright": "typecheck",
		"eslint": "lint", "biome": "lint", "ruff": "lint", "flake8": "lint", "pylint": "lint",
		"golangci-lint": "lint", "shellcheck": "lint", "ninja": "build",
	}
	scriptTestName = regexp.MustCompile(`(?i)^(?:run[-_]?)?(?:tests?|gate|verify)(?:[-_.][a-z0-9_-]*)?\.(?:sh|py)$|[-_](?:tests?|gate|verify)\.(?:sh|py)$`)
)

func classifyVerifySegment(seg []shTok) (string, string) {
	verb, args := verifyCommand(seg)
	if verb == "" {
		return "", ""
	}
	sub := func(valued map[string]bool) string { return firstPositional(args, valued) }
	switch verb {
	case "cargo":
		if c := cargoSub[sub(cargoValued)]; c != "" {
			return c, "cargo " + sub(cargoValued)
		}
	case "go":
		if c := goSub[sub(goValued)]; c != "" {
			return c, "go " + sub(goValued)
		}
	case "make", "just":
		if c := makeSub[sub(makeValued)]; c != "" {
			return c, verb + " " + sub(makeValued)
		}
	case "mvn", "gradle", "gradlew", "mvnw":
		if c := mvnSub[sub(gradleValued)]; c != "" {
			return c, verb + " " + sub(gradleValued)
		}
	case "dotnet", "swift", "deno":
		s := sub(nil)
		if verb == "deno" && s == "check" {
			return "typecheck", "deno check"
		}
		if c := dotSub[s]; c != "" {
			return c, verb + " " + s
		}
	case "xcodebuild":
		for _, a := range args {
			if c := dotSub[a.s]; c != "" && a.lit {
				return c, "xcodebuild " + a.s
			}
		}
	case "cmake":
		for _, a := range args {
			if a.lit && a.s == "--build" {
				return "build", "cmake --build"
			}
		}
	case "npm", "pnpm", "yarn", "bun":
		return classifyPackageScript(verb, args)
	case "bash", "sh", "zsh":
		if s := sub(nil); s != "" && scriptTestName.MatchString(filepath.Base(s)) {
			return "test", "test script"
		}
	}
	if strings.HasPrefix(verb, "python") {
		return classifyPython(args)
	}
	if c := bareVerb[verb]; c != "" {
		return c, verb
	}
	if scriptTestName.MatchString(verb) {
		return "test", "test script"
	}
	return "", ""
}

// classifyPackageScript reads `npm test`, `npm run lint`, `yarn build` and the
// like. A script name is mapped to a class word; the raw name is never emitted.
func classifyPackageScript(verb string, args []shTok) (string, string) {
	s := firstPositional(args, npmValued)
	switch s {
	case "test", "t":
		return "test", verb + " test"
	case "run", "run-script":
		rest := args
		for i, a := range args {
			if a.lit && a.s == s {
				rest = args[i+1:]
				break
			}
		}
		s = firstPositional(rest, npmValued)
	}
	if c := scriptClass(s); c != "" {
		return c, verb + " run " + c
	}
	return "", ""
}

func scriptClass(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "":
		return ""
	case strings.HasPrefix(n, "test"), strings.HasPrefix(n, "e2e"), n == "verify", n == "ci", n == "gate":
		return "test"
	case strings.HasPrefix(n, "typecheck"), strings.HasPrefix(n, "type-check"), n == "tsc", n == "check-types", n == "check":
		return "typecheck"
	case strings.HasPrefix(n, "lint"):
		return "lint"
	case strings.HasPrefix(n, "build"):
		return "build"
	}
	return ""
}

func classifyPython(args []shTok) (string, string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.lit {
			return "", ""
		}
		if a.s == "-m" && i+1 < len(args) && args[i+1].lit {
			mod := args[i+1].s
			if c := pyMods[mod]; c != "" {
				return c, "python -m " + mod
			}
			return "", ""
		}
		if pythonValued[a.s] {
			i++
			continue
		}
		if strings.HasPrefix(a.s, "-") {
			continue
		}
		// First positional is a script path.
		if scriptTestName.MatchString(filepath.Base(a.s)) || strings.HasPrefix(filepath.Base(a.s), "test_") {
			return "test", "test script"
		}
		return "", ""
	}
	return "", ""
}

// verifyCommand walks env assignments and runner wrappers (timeout 600,
// nice -n 10, uv run, npx, pnpm exec, ...) to the command that does the work.
func verifyCommand(seg []shTok) (string, []shTok) {
	i := 0
	for i < len(seg) && envAssign.MatchString(seg[i].s) {
		i++
	}
	toks := seg[i:]
	for depth := 0; depth < 6 && len(toks) > 0; depth++ {
		if !toks[0].lit {
			return "", nil
		}
		v := filepath.Base(toks[0].s)
		if !verbChars.MatchString(v) {
			return "", nil
		}
		rest := toks[1:]
		switch v {
		case "env":
			for len(rest) > 0 && (envAssign.MatchString(rest[0].s) || strings.HasPrefix(rest[0].s, "-")) {
				rest = rest[1:]
			}
		case "time", "nohup", "command", "exec", "builtin":
		case "nice", "ionice", "sudo", "stdbuf", "chrt":
			rest = skipFlags(rest, map[string]bool{"-n": true, "-c": true, "-u": true, "-g": true, "-p": true})
		case "timeout":
			rest = skipFlags(rest, map[string]bool{"-k": true, "-s": true, "--kill-after": true, "--signal": true})
			if len(rest) > 0 {
				rest = rest[1:] // the duration
			}
		case "npx", "bunx", "pnpx":
			rest = skipFlags(rest, map[string]bool{"-p": true, "--package": true})
		case "uv", "poetry", "pipenv", "pdm", "hatch", "rye":
			if firstPositional(rest, nil) != "run" {
				return v, rest
			}
			rest = skipFlags(afterToken(rest, "run"), map[string]bool{"--with": true, "--python": true, "-p": true, "--project": true, "--directory": true})
		case "pnpm", "yarn":
			s := firstPositional(rest, npmValued)
			if s != "exec" && s != "dlx" {
				return v, rest
			}
			rest = skipFlags(afterToken(rest, s), nil)
		default:
			return v, rest
		}
		toks = rest
	}
	return "", nil
}

// skipFlags drops leading flags; a flag in valued consumes the next token
// unless written as --flag=value.
func skipFlags(toks []shTok, valued map[string]bool) []shTok {
	for len(toks) > 0 && strings.HasPrefix(toks[0].s, "-") && toks[0].s != "-" {
		f := toks[0].s
		toks = toks[1:]
		if valued[f] && !strings.Contains(f, "=") && len(toks) > 0 {
			toks = toks[1:]
		}
	}
	return toks
}

// firstPositional returns the first literal non-flag token, skipping the
// values of valued flags. "" when the first positional is not literal.
func firstPositional(toks []shTok, valued map[string]bool) string {
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t.s, "-") || strings.HasPrefix(t.s, "+") {
			if valued[t.s] && i+1 < len(toks) {
				i++
			}
			continue
		}
		if !t.lit {
			return ""
		}
		return t.s
	}
	return ""
}

func afterToken(toks []shTok, s string) []shTok {
	for i, t := range toks {
		if t.lit && t.s == s {
			return toks[i+1:]
		}
	}
	return nil
}

// --- git operations ---------------------------------------------------------

// gitSubcommands are the git operations reported on the bus: plain words from
// a fixed table, so no path, ref or commit message is emitted. Commit and
// revert detection in the reflex detectors key on these.
var gitSubcommands = map[string]bool{
	"add": true, "commit": true, "push": true, "pull": true, "fetch": true,
	"checkout": true, "switch": true, "restore": true, "reset": true, "revert": true,
	"stash": true, "merge": true, "rebase": true, "cherry-pick": true, "worktree": true,
	"status": true, "diff": true, "log": true, "show": true, "branch": true, "tag": true,
	"clean": true, "rm": true, "mv": true, "apply": true, "am": true,
}

var gitValued = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// bashGitOps returns "git <sub>" for each git segment of a command, deduped
// in order: "git add git commit git push".
func bashGitOps(cmd string) string {
	var ops []string
	seen := map[string]bool{}
	for _, seg := range splitShell(cmd) {
		verb, args := verifyCommand(seg)
		if verb != "git" {
			continue
		}
		sub := firstPositional(args, gitValued)
		if !gitSubcommands[sub] {
			continue
		}
		op := "git " + sub
		if seen[op] {
			continue
		}
		seen[op] = true
		ops = append(ops, op)
		if len(ops) == maxActivityVerbs {
			break
		}
	}
	return strings.Join(ops, " ")
}

// --- outcome ----------------------------------------------------------------

// verifyFailMarkers are runner lines that mean a verify failed even when the
// shell exited 0, e.g. `cargo test 2>&1 | tail -5`. Anchored to runner
// formats; a bare "error" in passing output does not count.
var verifyFailMarkers = regexp.MustCompile(`(?m)` + strings.Join([]string{
	`^\s*FAIL\b`,
	`^\s*--- FAIL:`,
	`^FAILED\b`,
	`test result: FAILED`,
	`\b[1-9]\d*\s+(?:failed|failing|errors?)\b`,
	`Tests?:\s+[1-9]\d*\s+failed`,
	`error\[E\d{4}\]`,
	`^error: could not compile`,
	`BUILD FAILED`,
	`^npm ERR!`,
	`^\s*ELIFECYCLE`,
	`error TS\d+:`,
	`Found [1-9]\d* errors?`,
	`^make(?:\[\d+\])?: \*\*\*`,
}, "|"))

// Scan bounds: runner summaries sit at the head or the tail of the output.
const (
	verifyScanHead = 16 << 10
	verifyScanTail = 64 << 10
)

func boundedScan(s string) bool {
	if len(s) <= verifyScanHead+verifyScanTail {
		return verifyFailMarkers.MatchString(s)
	}
	return verifyFailMarkers.MatchString(s[:verifyScanHead]) || verifyFailMarkers.MatchString(s[len(s)-verifyScanTail:])
}

type bashResponse struct {
	Stdout           string `json:"stdout"`
	Stderr           string `json:"stderr"`
	Interrupted      bool   `json:"interrupted"`
	BackgroundTaskID string `json:"backgroundTaskId"`
}

// isToolFailure reports whether the hook fired for a failed tool call.
func isToolFailure(in event.HookInput) bool {
	return in.HookEventName == "PostToolUseFailure"
}

var exitCodeRe = regexp.MustCompile(`^Exit code (-?\d+)`)

// failureErrorType labels a PostToolUseFailure: interrupt, exit (a Bash
// non-zero exit), or error. Never the error text.
func failureErrorType(in event.HookInput) string {
	switch {
	case in.IsInterrupt:
		return "interrupt"
	case in.ToolName == "Bash" && exitCodeRe.MatchString(in.Error):
		return "exit"
	}
	return "error"
}

// failureResponseSummary replaces tool_response_summary on a failed call,
// which has no tool_response: size of the error, its exit code, and the
// verify outcome when the command was a verification.
func failureResponseSummary(in event.HookInput) string {
	out := map[string]any{"bytes": len(in.Error), "is_error": true}
	if m := exitCodeRe.FindStringSubmatch(in.Error); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out["exit_code"] = n
		}
	}
	if in.IsInterrupt {
		out["interrupted"] = true
	}
	if in.ToolName == "Bash" {
		if class, _ := classifyVerify(claimGateBashCommand(in.ToolInput)); class != "" {
			if in.IsInterrupt {
				out["verify_outcome"] = "unknown"
			} else {
				out["verify_outcome"] = "fail"
			}
		}
	}
	return marshalSummary(out)
}

// bashVerifyOutcome reads a successful Bash response: pass, or fail when a
// runner marker survived an exit-0 pipe. unknown for a background launch or
// an interrupt. "" when the command is not a verification.
func bashVerifyOutcome(command string, raw json.RawMessage) (outcome string, interrupted, background bool) {
	var r bashResponse
	_ = json.Unmarshal(raw, &r)
	if class, _ := classifyVerify(command); class == "" {
		return "", r.Interrupted, r.BackgroundTaskID != ""
	}
	switch {
	case r.BackgroundTaskID != "", r.Interrupted:
		outcome = "unknown"
	case boundedScan(r.Stdout) || boundedScan(r.Stderr):
		outcome = "fail"
	default:
		outcome = "pass"
	}
	return outcome, r.Interrupted, r.BackgroundTaskID != ""
}
