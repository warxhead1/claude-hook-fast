// cmd_brief_lint.go — PreToolUse hook for Agent|Task|Workflow dispatch.
//
// Lints the dispatch brief for text that steered agents into known harness
// denials or standing-context waste, and tells the DISPATCHING model on its
// next request via hookSpecificOutput.additionalContext. It never blocks and
// never rewrites tool_input (no permissionDecision, no updatedInput), so it
// composes with dispatch-freeze-guard and kb-dispatch-inject, which do
// rewrite. Fail-open everywhere: any parse error or unrelated tool exits 0
// with no stdout.
//
// Each check maps to a denial or cost family that nervous-bus's guard_denial
// detector measures (claude-hook-fast#48, nervous-bus#140). Checks fire only
// on text the brief itself carries; command shapes an agent invents on its
// own are the detector's job, not this hook's.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

const (
	blMaxText       = 512 << 10 // lint at most this much brief/script text
	blMaxFindings   = 5
	blSnippetChars  = 60
	blPreambleBytes = 1536 // shared-preamble budget, dispatch brief template v1
	blMinSleepSec   = 30
	// How far back from a preamble reference to look for the arrow of a
	// per-lane prompt builder.
	blBuilderLookback = 800
)

type briefFinding struct {
	ID      string
	Snippet string
	Fix     string
}

// Wrapper commands only count in command position: the word "nice" in prose
// is not a finding, "nice -n 10" is.
const blExe = `(?:(?:python3?|bash|sh|nohup|sqlite3|go|cargo|make|npm|node|pytest|uv)\b|[./~$][^\s` + "`" + `'"]*)`

var (
	blWrapperRE = regexp.MustCompile(`\b(?:nice\s+(?:-n\s*-?\d+|-\d+|--adjustment|` + blExe + `)|ionice\s+-c\s*\d|setsid\s+(?:-[a-z]+\s+)*` + blExe + `)`)
	blGitPipeRE = regexp.MustCompile(`\|\s*git\s+[a-z]|\$\(\s*git\s+[a-z]`)
	blReportRE  = regexp.MustCompile(`(?i)\breport\.md\b|\b(?:write|save|put)\s+(?:the\s+|a\s+|your\s+)?(?:full\s+|final\s+)?(?:report|findings|write-up)\s+(?:to|in|into|at|under)\b`)
	blSleepRE   = regexp.MustCompile(`\bsleep\s+(\d+)([smh]?)\b`)
	blBaseRE    = regexp.MustCompile(`\borigin/[A-Za-z0-9._/-]+`)
	blIsoRE     = regexp.MustCompile(`isolation\s*:\s*['"]worktree['"]`)
	blConstRE   = regexp.MustCompile(`(?m)^\s*(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*` + "`")
	blNegRE     = regexp.MustCompile(`(?i)\b(?:don'?t|do not|never|no|not|avoid|instead of|without|stop|refus\w*|blocked|denied)\b`)
	blListRE    = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s`)
	blBuilderRE = regexp.MustCompile(`=>|\bfunction\b\s*[\w$]*\s*\(\s*[\w$]`)
)

// briefLint returns the findings for one dispatch. text is the Agent prompt or
// the Workflow script; isolated says whether the agent(s) run in a worktree.
func briefLint(tool, text, agentType string, isolated bool) []briefFinding {
	if len(text) > blMaxText {
		text = text[:blMaxText]
	}
	var out []briefFinding
	add := func(f briefFinding) {
		for _, g := range out {
			if g.ID == f.ID {
				return // one finding per check is enough to act on
			}
		}
		out = append(out, f)
	}

	if isolated {
		if s, ok := blFirstLive(text, blWrapperRE); ok {
			add(briefFinding{"wrapper", s,
				"isolated agents get nice/ionice/setsid denied by the worktree guard (26 of 116 guard denials, 2026-11-08 waves). Drop the wrapper: game-priority-watch already sets agent priority."})
		}
		if s, ok := blFirstLive(text, blGitPipeRE); ok {
			add(briefFinding{"git_pipe", s,
				"the worktree guard cannot verify git inside a pipe or $(...) and denies it. Write the git output to a file first (git diff A...B -- f > scratch/d.patch), then run git apply -R scratch/d.patch."})
		}
		if !blBaseRE.MatchString(text) {
			add(briefFinding{"base_branch", "(no origin/<branch> in the brief)",
				"worktree agents branch from origin/HEAD, not from your branch. Name the base and have the agent run git merge --ff-only origin/<branch> first."})
		}
	}
	if agentType != "report-writer" {
		if s, ok := blFirstLive(text, blReportRE); ok {
			add(briefFinding{"report_file", s,
				"Claude Code blocks subagent report-file writes ('return findings as text'; 12 blocked report.md writes in 8 lanes). Ask for the findings in the final text or structured return and persist them yourself."})
		}
	}
	for _, m := range blSleepRE.FindAllStringSubmatchIndex(text, -1) {
		if blNegated(text, m[0]) {
			continue
		}
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		switch text[m[4]:m[5]] {
		case "m":
			n *= 60
		case "h":
			n *= 3600
		}
		if n >= blMinSleepSec {
			add(briefFinding{"sleep_wait", blSnippet(text, m[0]),
				"Claude Code blocks long sleeps in agents (9 blocked waits in 7 lanes). Tell the agent to start long jobs with run_in_background: true; it is woken when they exit."})
			break
		}
	}
	if tool == "Workflow" {
		for _, p := range blSharedPreambles(text) {
			add(briefFinding{"preamble", fmt.Sprintf("%s is %.1f KB, spliced into lane prompts (%d refs)", p.name, float64(p.bytes)/1024, p.uses),
				fmt.Sprintf("a shared preamble rides along on every request of every agent that gets it. Keep it under %.1f KB and name only the rules each lane needs; CLAUDE.md is already injected.", float64(blPreambleBytes)/1024)})
			break
		}
	}
	if len(out) > blMaxFindings {
		out = out[:blMaxFindings]
	}
	return out
}

// blFirstLive returns the first match of re that is not in a negated sentence
// ("don't use nice -n 10", or a list item under "Avoid these:").
func blFirstLive(text string, re *regexp.Regexp) (string, bool) {
	for _, m := range re.FindAllStringIndex(text, -1) {
		if !blNegated(text, m[0]) {
			return blSnippet(text, m[0]), true
		}
	}
	return "", false
}

// blNegated reports whether the sentence holding pos says not to do it.
// The sentence runs from the last line break or sentence end before pos.
// A list item also inherits the negation of its lead-in line.
func blNegated(text string, pos int) bool {
	lineStart := strings.LastIndexByte(text[:pos], '\n') + 1
	sent := lineStart
	for _, sep := range []string{". ", "? ", "! ", "; "} {
		if i := strings.LastIndex(text[lineStart:pos], sep); i >= 0 && lineStart+i+len(sep) > sent {
			sent = lineStart + i + len(sep)
		}
	}
	if blNegRE.MatchString(text[sent:pos]) {
		return true
	}
	if !blListRE.MatchString(text[lineStart:pos]) {
		return false
	}
	// Walk up over sibling list items to the lead-in line.
	end := lineStart - 1
	for end > 0 {
		start := strings.LastIndexByte(text[:end], '\n') + 1
		line := text[start:end]
		if strings.TrimSpace(line) == "" {
			return false
		}
		if !blListRE.MatchString(line) {
			return blNegRE.MatchString(line)
		}
		end = start - 1
	}
	return false
}

func blSnippet(text string, pos int) string {
	end := pos + blSnippetChars
	if end > len(text) {
		end = len(text)
	}
	s := text[pos:end]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

type blPreamble struct {
	name  string
	bytes int
	uses  int
}

// blSharedPreambles finds template-literal constants over the preamble budget
// that reach more than one agent: referenced twice or more, or once inside a
// per-lane builder (an arrow or a function with a parameter within
// blBuilderLookback bytes before the reference, e.g. `l => agent(`${PRE}...`
// or `function fixPrompt(l) { return `${PRE}...` }`).
// A single top-level `await agent(CRITIC, ...)` is not a shared preamble.
func blSharedPreambles(script string) []blPreamble {
	var out []blPreamble
	for _, m := range blConstRE.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		body, end, ok := blTemplateLiteral(script, m[1])
		if !ok || len(body) <= blPreambleBytes {
			continue
		}
		refs := regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).FindAllStringIndex(script[end:], -1)
		shared := len(refs) >= 2
		if len(refs) == 1 {
			at := end + refs[0][0]
			from := at - blBuilderLookback
			if from < end {
				from = end
			}
			shared = blBuilderRE.MatchString(script[from:at])
		}
		if shared {
			out = append(out, blPreamble{name, len(body), len(refs)})
		}
	}
	return out
}

// blTemplateLiteral scans a JS template literal whose opening backtick sits
// just before start. It returns the raw body and the index after the closing
// backtick. ${...} spans are skipped by brace depth; a nested template
// literal inside one aborts the scan (ok=false), which only drops a finding.
func blTemplateLiteral(s string, start int) (string, int, bool) {
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			return s[start:i], i + 1, true
		case '$':
			if i+1 < len(s) && s[i+1] == '{' {
				depth := 0
				for i++; i < len(s); i++ {
					switch s[i] {
					case '{':
						depth++
					case '}':
						depth--
					case '`':
						return "", 0, false
					}
					if depth == 0 {
						break
					}
				}
			}
		}
	}
	return "", 0, false
}

// blRender is the additionalContext text the dispatching model sees.
func blRender(findings []briefFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "brief-lint (claude-hook-fast#48): this dispatch's brief has %d pattern(s) that cost agents denials or context in past waves. The dispatch went out unchanged. Fix them in the next brief:", len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "\n- %s `%s`: %s", f.ID, f.Snippet, f.Fix)
	}
	return b.String()
}

func blOutput(findings []briefFinding) []byte {
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PreToolUse",
			"additionalContext": blRender(findings),
		},
	})
	return out
}

// blReadScript returns a Workflow's script text, from `script` or, when the
// call iterates on a saved file, from `scriptPath`.
func blReadScript(ti map[string]any) string {
	if s, _ := ti["script"].(string); s != "" {
		return s
	}
	p, _ := ti["scriptPath"].(string)
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, blMaxText)
	n, _ := f.Read(buf)
	return string(buf[:n])
}

func cmdBriefLint() {
	input, ok := hookio.ReadHookInput()
	if !ok || len(input.ToolInput) == 0 {
		return
	}
	var ti map[string]any
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
		return
	}
	var text, agentType string
	var isolated bool
	switch input.ToolName {
	case "Agent", "Task":
		text, _ = ti["prompt"].(string)
		agentType, _ = ti["subagent_type"].(string)
		iso, _ := ti["isolation"].(string)
		isolated = iso == "worktree"
	case "Workflow":
		text = blReadScript(ti)
		isolated = blIsoRE.MatchString(text)
	default:
		return
	}
	if text == "" {
		return
	}
	findings := briefLint(input.ToolName, text, agentType, isolated)
	if len(findings) == 0 {
		return
	}
	ids := make([]string, len(findings))
	for i, f := range findings {
		ids[i] = f.ID
	}
	hooklog.Writef("brief-lint: tool=%s isolated=%t findings=%s", input.ToolName, isolated, strings.Join(ids, ","))
	fmt.Fprintln(os.Stdout, string(blOutput(findings)))
}
