package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func blIDs(fs []briefFinding) string {
	ids := make([]string, len(fs))
	for i, f := range fs {
		ids[i] = f.ID
	}
	return strings.Join(ids, ",")
}

func blHas(fs []briefFinding, id string) bool {
	for _, f := range fs {
		if f.ID == id {
			return true
		}
	}
	return false
}

// Lines the 2026-11-08 fix wave (wf_3998f149-2b7) actually sent to its
// isolated lanes. The worktree guard then denied 116 calls across all 21 of
// them; nice/setsid wrappers alone were 26.
const blV0FixWave = `const COMMON = ` + "`" + `
Base: git fetch origin && git merge --ff-only origin/main first.
Stores are read-only: open them as file:<path>?mode=ro; use nice -n 10 for heavy scans.
` + "`" + `
const review = (l) => agent(` + "`" + `${COMMON}
(3) NON-VACUITY: for each new test, reverse-apply only the source change ('git diff origin/main...HEAD -- <source files> | git apply -R'), run the test, confirm it FAILS.
` + "`" + `, {model: 'sonnet', isolation: 'worktree'})
`

// The 2026-11-08 grounding wave (wf_e3601a15-baf) asked every lane for a
// report.md; Claude Code blocked 12 of those writes in 8 lanes.
const blV0Grounding = "Your lane dir: ${EV}/${l.key}/ — write report.md there (full detail, tables, method, caveats).\n" +
	"l => agent(`${OSS_PRE}\\n\\n${l.body}\\n\\nWrite the full report to ${EV}/${l.key}/report.md and return report_path.`)"

func TestBriefLint_V0FixWaveFiresOnWhatItShipped(t *testing.T) {
	fs := briefLint("Workflow", blV0FixWave, "", blIsoRE.MatchString(blV0FixWave))
	if !blHas(fs, "wrapper") || !blHas(fs, "git_pipe") {
		t.Fatalf("v0 fix wave: want wrapper and git_pipe, got %q", blIDs(fs))
	}
	if blHas(fs, "base_branch") {
		t.Fatalf("v0 fix wave names origin/main; base_branch must not fire: %q", blIDs(fs))
	}
}

func TestBriefLint_V0GroundingFiresReportFile(t *testing.T) {
	fs := briefLint("Workflow", blV0Grounding, "", false)
	if !blHas(fs, "report_file") {
		t.Fatalf("v0 grounding: want report_file, got %q", blIDs(fs))
	}
	// Not isolated: worktree-guard checks stay quiet.
	if blHas(fs, "wrapper") || blHas(fs, "base_branch") || blHas(fs, "git_pipe") {
		t.Fatalf("non-isolated brief got worktree checks: %q", blIDs(fs))
	}
}

// A brief written to the v1 template, including the lines that NAME the
// forbidden shapes in order to forbid them, must lint clean.
const blV1Clean = `QUESTION: how many tool calls does each run lose to denials?
BASE: start with git fetch origin and git merge --ff-only origin/main.
COMMAND SHAPES. The worktree guard denied 116 calls in the last wave. Avoid these:
- nice/ionice/setsid wrappers, e.g. ` + "`nice -n 10 python3 q.py`" + `.
- ` + "`sqlite3 'file:…'`" + ` operands. Open stores from a python script instead.
- git inside heredocs, pipes or $(…), e.g. ` + "`git diff | git apply -R`" + `.
- sleep waits such as ` + "`sleep 60`" + `. Use run_in_background.
Don't write report.md; return findings as your final text.
Never use setsid python3 server.py.
RETURN: the PR URL and the gate output tail.`

func TestBriefLint_V1TemplateBriefIsClean(t *testing.T) {
	if fs := briefLint("Agent", blV1Clean, "sonnet-worker", true); len(fs) != 0 {
		t.Fatalf("v1 brief should be clean, got %q: %+v", blIDs(fs), fs)
	}
}

func TestBriefLint_IsolatedAgentWithoutBaseBranch(t *testing.T) {
	fs := briefLint("Agent", "Fix the flaky test in pkg/x and push.", "sonnet-worker", true)
	if blIDs(fs) != "base_branch" {
		t.Fatalf("want base_branch only, got %q", blIDs(fs))
	}
	if fs := briefLint("Agent", "Fix the flaky test in pkg/x and push.", "sonnet-worker", false); len(fs) != 0 {
		t.Fatalf("non-isolated agent needs no base branch, got %q", blIDs(fs))
	}
}

func TestBriefLint_WrapperNeedsCommandPosition(t *testing.T) {
	for _, prose := range []string{
		"That is a nice touch. Base: origin/main.",
		"Keep it nice and short. Base: origin/main.",
		"The setsid wrappers are covered in #106. Base: origin/main.",
	} {
		if fs := briefLint("Agent", prose, "", true); len(fs) != 0 {
			t.Errorf("prose %q flagged: %q", prose, blIDs(fs))
		}
	}
	for _, cmd := range []string{
		"Run nice -n 10 pytest -q. Base: origin/main.",
		"Run `nice python3 big.py`. Base: origin/main.",
		"Start it with setsid -f ./serve.sh. Base: origin/main.",
		"Use ionice -c3 tar cf x.tar d. Base: origin/main.",
	} {
		if fs := briefLint("Agent", cmd, "", true); !blHas(fs, "wrapper") {
			t.Errorf("command %q not flagged: %q", cmd, blIDs(fs))
		}
	}
}

func TestBriefLint_ReportWriterMayWriteReports(t *testing.T) {
	brief := "Audit the hooks and write the full report to docs/audit.md."
	if fs := briefLint("Agent", brief, "report-writer", false); len(fs) != 0 {
		t.Fatalf("report-writer exists to write reports, got %q", blIDs(fs))
	}
	if fs := briefLint("Agent", brief, "sonnet-worker", false); !blHas(fs, "report_file") {
		t.Fatalf("sonnet-worker report request not flagged: %q", blIDs(fs))
	}
	// Allowed writes from the measured waves: pr_body.md, commit.txt, scripts.
	ok := "Write the PR body to scratch/pr_body.md and the commit message to scratch/commit.txt."
	if fs := briefLint("Agent", ok, "sonnet-worker", false); len(fs) != 0 {
		t.Fatalf("non-report writes flagged: %q", blIDs(fs))
	}
}

func TestBriefLint_SleepThreshold(t *testing.T) {
	cases := map[string]bool{
		"After the build, sleep 60 and then read the log.": true,
		"sleep 2m then check":                               true,
		"Run sleep 2 && curl -s localhost:5173 to settle.":  false,
		"Do not sleep 60 between checks.":                   false,
	}
	for brief, want := range cases {
		if got := blHas(briefLint("Agent", brief, "", false), "sleep_wait"); got != want {
			t.Errorf("%q: sleep_wait=%t, want %t", brief, got, want)
		}
	}
}

func TestBriefLint_SharedPreamble(t *testing.T) {
	big := strings.Repeat("rule line that every lane gets pasted in.\n", 60) // ~2.5 KB
	small := strings.Repeat("x", 1000)
	fanout := "const PRE = `" + big + "`\nconst lanes = []\nawait pipeline(lanes, l => agent(`${PRE}\\n${l.body}`, {model: 'sonnet'}))\n"
	if fs := briefLint("Workflow", fanout, "", false); !blHas(fs, "preamble") {
		t.Fatalf("2.5 KB preamble in a per-lane builder not flagged: %q", blIDs(fs))
	}
	// The v0 fix wave's shape: a 2.9 KB COMMON spliced once, inside
	// function fixPrompt(l), which pipeline() calls per lane.
	fn := "const COMMON = `" + big + "`\nfunction fixPrompt(l) {\n  return `LANE ${l.key}\n${COMMON.split('<lane>').join(l.key)}`\n}\n"
	if fs := briefLint("Workflow", fn, "", false); !blHas(fs, "preamble") {
		t.Fatalf("preamble inside function fixPrompt(l) not flagged: %q", blIDs(fs))
	}
	twice := "const PRE = `" + big + "`\nawait agent(`${PRE} a`, {model: 'sonnet'})\nawait agent(`${PRE} b`, {model: 'sonnet'})\n"
	if fs := briefLint("Workflow", twice, "", false); !blHas(fs, "preamble") {
		t.Fatalf("2.5 KB preamble used twice not flagged: %q", blIDs(fs))
	}
	under := "const PRE = `" + small + "`\nawait pipeline(lanes, l => agent(`${PRE}${l.body}`, {model: 'sonnet'}))\n"
	if fs := briefLint("Workflow", under, "", false); blHas(fs, "preamble") {
		t.Fatalf("1 KB preamble flagged: %q", blIDs(fs))
	}
	// A long single-purpose prompt for one top-level agent is not a preamble.
	critic := "const CRITIC = `" + big + "`\n\n\n" + strings.Repeat("// spacer\n", 100) + "const c = await agent(CRITIC, {model: 'opus'})\n"
	if fs := briefLint("Workflow", critic, "", false); blHas(fs, "preamble") {
		t.Fatalf("single-use critic prompt flagged: %q", blIDs(fs))
	}
	// Interpolations inside the preamble are skipped, not mistaken for its end.
	nested := "const PRE = `head ${EV}/${'x'} " + big + "`\nawait pipeline(lanes, l => agent(`${PRE}`, {model: 'sonnet'}))\n"
	if fs := briefLint("Workflow", nested, "", false); !blHas(fs, "preamble") {
		t.Fatalf("preamble with ${} interpolation not measured: %q", blIDs(fs))
	}
}

func TestBriefLint_OutputIsNonBlockingAdditionalContext(t *testing.T) {
	fs := briefLint("Workflow", blV0FixWave, "", true)
	var got map[string]map[string]any
	if err := json.Unmarshal(blOutput(fs), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	hso := got["hookSpecificOutput"]
	if hso["hookEventName"] != "PreToolUse" {
		t.Fatalf("hookEventName = %v", hso["hookEventName"])
	}
	for _, k := range []string{"permissionDecision", "updatedInput"} {
		if _, ok := hso[k]; ok {
			t.Fatalf("lint must not set %s (it would block or clobber sibling hooks)", k)
		}
	}
	ctx, _ := hso["additionalContext"].(string)
	if !strings.Contains(ctx, "wrapper `") || !strings.Contains(ctx, "went out unchanged") {
		t.Fatalf("additionalContext missing finding detail:\n%s", ctx)
	}
	if len(ctx) > 2048 {
		t.Fatalf("additionalContext is %d bytes; keep it bounded", len(ctx))
	}
}

func TestBriefLint_NegatedListLeadIn(t *testing.T) {
	brief := "Base: origin/main.\nNever do any of these:\n- nice -n 10 python3 x.py\n- setsid ./serve.sh\n\nThen:\n- run pytest"
	if fs := briefLint("Agent", brief, "", true); len(fs) != 0 {
		t.Fatalf("list under a negated lead-in flagged: %q", blIDs(fs))
	}
	live := "Base: origin/main.\nSteps:\n- nice -n 10 python3 x.py"
	if fs := briefLint("Agent", live, "", true); !blHas(fs, "wrapper") {
		t.Fatalf("list under a neutral lead-in not flagged: %q", blIDs(fs))
	}
}
