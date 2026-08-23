// cmd_kb_dispatch_inject.go — PreToolUse hook for Agent|Task dispatch:
// prepends a bounded "## KB Context (auto-injected)" markdown block to the
// dispatch prompt, giving subagents the kb-vault context that
// SessionStart's kb-prime-siblings hook (cmd_kb_prime_siblings.go) never
// reaches — SessionStart does NOT fire for dispatched subagents, so today
// they brief with zero kb context.
//
// Mechanism: same hookSpecificOutput.updatedInput contract
// cmd_dispatch_freeze_guard.go already uses to rewrite dispatch input
// (exit 0 + JSON on stdout, see replaceRootRefs there). We only ever
// rewrite the "prompt" field; the rest of tool_input passes through
// unmodified.
//
// Guards (all four required, see mission doc):
//
//	(a) no double-inject: skip when the prompt already contains the
//	    "KB Context (auto-injected)" marker (retries, forks, or an
//	    upstream hook that already injected it).
//	(b) FAIL-OPEN: any kb error/timeout/empty-result returns the input
//	    unmodified (no stdout at all — the harness treats a silent exit 0
//	    as "no change").
//	(c) Workflow tool calls are skipped entirely in v1. A Workflow's
//	    "script" field drives a multi-agent orchestration, not a single
//	    prompt string — there is no single place to prepend a context
//	    block that reaches every spawned agent's brief, and rewriting the
//	    script text risks corrupting the orchestration DSL. Revisit if/
//	    when Workflow gains a per-agent prompt-template hook.
//	(d) pure-mechanical subagent_types (kdiSkipSubagentTypes) never get
//	    kb context — they don't need it and it only inflates their input.
//
// kb-brief feature detection: a sibling lane is landing `kb brief`
// tonight as a purpose-built dispatch-briefing verb. We probe for it
// (`kb brief --help`, tight timeout) and prefer it when present, but
// never depend on it — `kb prime --project <p> --json` (already used by
// cmd_kb_prime_siblings.go) is the fallback and reuses that file's
// kpsPrimeResult/kpsPrimeEntry parsing.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

const (
	// kdiMarker is the double-inject guard string. Must match the header
	// kdiRenderBlock actually emits.
	kdiMarker = "KB Context (auto-injected)"

	// kdiOverallTimeout bounds the whole hook: the optional `kb brief
	// --help` feature probe plus the main kb call share this deadline.
	// PreToolUse hooks sit directly in the critical path of every
	// dispatch, so this must stay tight.
	kdiOverallTimeout = 3 * time.Second

	// kdiHelpProbeTimeout bounds just the `kb brief --help` feature
	// probe, carved out of kdiOverallTimeout so a hung probe can't eat
	// the whole budget and starve the real call.
	kdiHelpProbeTimeout = 500 * time.Millisecond

	// kdiBlockCapBytes bounds the rendered KB Context block (~2KB per
	// the mission spec) — belt and suspenders against context bloat on
	// every single dispatch, not just the sibling-activity case
	// cmd_kb_prime_siblings.go guards.
	kdiBlockCapBytes = 2000

	// kdiTitleCapRunes bounds one rendered entry title.
	kdiTitleCapRunes = 100

	// kdiMaxItemsPerSection bounds how many entries render per section
	// (Active Plans / Recent / Gaps / Pending Vets).
	kdiMaxItemsPerSection = 5
)

// kdiSkipSubagentTypes are dispatch targets that do pure-mechanical work
// and never benefit from kb vault context — injecting it only inflates
// their input for no gain.
var kdiSkipSubagentTypes = map[string]bool{
	"haiku-grunt":      true,
	"statusline-setup": true,
}

// kdiHasBriefVerb probes `kb brief --help` with a tight timeout and
// reports whether it succeeded. Any failure (missing subcommand, error,
// timeout) reports false — callers fall back to `kb prime`.
func kdiHasBriefVerb(ctx context.Context, binary string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, kdiHelpProbeTimeout)
	defer cancel()
	cmd := osexec.CommandContext(probeCtx, binary, "brief", "--help") //nolint:gosec
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// kdiFetchContext shells out to `kb <verb> --project <p> --json` and
// parses the result into a kpsPrimeResult (shared with
// cmd_kb_prime_siblings.go — both consume the same prime/brief JSON
// shape). ok=false on any failure; callers must fail open.
func kdiFetchContext(ctx context.Context, binary, verb, proj string) (kpsPrimeResult, bool) {
	var result kpsPrimeResult
	cmd := osexec.CommandContext(ctx, binary, verb, "--project", proj, "--json") //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return result, false
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		return result, false
	}
	return result, true
}

// kdiRenderSection formats one bounded section ("### <label>") from a
// list of titles, capped at kdiMaxItemsPerSection entries and
// kdiTitleCapRunes per entry. Returns "" when titles is empty.
func kdiRenderSection(label string, titles []string) string {
	if len(titles) == 0 {
		return ""
	}
	n := len(titles)
	if n > kdiMaxItemsPerSection {
		n = kdiMaxItemsPerSection
	}
	var lines []string
	for _, t := range titles[:n] {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		lines = append(lines, "- "+kpsTruncateRunes(t, kdiTitleCapRunes))
	}
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("### %s\n%s", label, strings.Join(lines, "\n"))
}

// kdiEntryTitles extracts .Title from a []kpsPrimeEntry, skipping blanks.
func kdiEntryTitles(entries []kpsPrimeEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Title != "" {
			out = append(out, e.Title)
		}
	}
	return out
}

// kdiRenderBlock assembles the full "## KB Context (auto-injected)"
// block from a kpsPrimeResult, bounded to kdiBlockCapBytes. Returns ""
// when the result has nothing to show — callers must not inject an
// empty block.
func kdiRenderBlock(r kpsPrimeResult) string {
	var sections []string
	if s := kdiRenderSection("Active Plans", kdiEntryTitles(r.ActivePlans)); s != "" {
		sections = append(sections, s)
	}
	if len(r.PendingVets) > 0 {
		label := fmt.Sprintf("Pending Vets (%d)", len(r.PendingVets))
		if s := kdiRenderSection(label, r.PendingVets); s != "" {
			sections = append(sections, s)
		}
	}
	if s := kdiRenderSection("Recent", kdiEntryTitles(r.Recent)); s != "" {
		sections = append(sections, s)
	}
	if s := kdiRenderSection("Open Gaps", kdiEntryTitles(r.Gaps)); s != "" {
		sections = append(sections, s)
	}
	if len(sections) == 0 {
		return ""
	}

	block := "## " + kdiMarker + "\n\n" + strings.Join(sections, "\n\n")
	if len(block) > kdiBlockCapBytes {
		// Drop whole sections from the end until it fits, rather than
		// cutting mid-line — mirrors kpsRenderSiblingSection's approach.
		for len(sections) > 1 {
			sections = sections[:len(sections)-1]
			block = "## " + kdiMarker + "\n\n" + strings.Join(sections, "\n\n")
			if len(block) <= kdiBlockCapBytes {
				break
			}
		}
		if len(block) > kdiBlockCapBytes {
			block = kpsTruncateRunes(block, kdiBlockCapBytes)
		}
	}
	return block
}

// cmdKbDispatchInject is the PreToolUse hook entry point (matcher
// Agent|Task|Workflow). See package doc comment for the full design.
// FAIL-OPEN throughout: any early return before the final fmt.Println
// leaves the dispatch input untouched.
func cmdKbDispatchInject() {
	input, ok := hookio.ReadHookInput()
	if !ok || input == nil {
		return
	}
	if input.ToolName != "Agent" && input.ToolName != "Task" {
		// Workflow: skipped in v1, see package doc comment (c).
		return
	}

	var ti map[string]any
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil || ti == nil {
		return
	}

	if st, _ := ti["subagent_type"].(string); kdiSkipSubagentTypes[st] {
		return
	}

	prompt, _ := ti["prompt"].(string)
	if prompt == "" {
		return
	}
	if strings.Contains(prompt, kdiMarker) {
		return // guard (a): no double-inject
	}

	binary := resolveKbBinary()
	if binary == "" {
		return
	}

	proj, _ := project.Resolve(input.Cwd)
	if proj == "" || proj == "unknown" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), kdiOverallTimeout)
	defer cancel()

	verb := "prime"
	if kdiHasBriefVerb(ctx, binary) {
		verb = "brief"
	}

	result, ok := kdiFetchContext(ctx, binary, verb, proj)
	if !ok {
		return // guard (b): kb error/timeout -> fail open
	}

	block := kdiRenderBlock(result)
	if block == "" {
		return // nothing worth injecting
	}

	ti["prompt"] = block + "\n\n" + prompt
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  ti,
	}})
	if err != nil {
		return
	}
	fmt.Println(string(out))
}
