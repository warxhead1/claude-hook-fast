package main

import (
	"os"
	"strings"
	"testing"
)

// ---------- prFormatAge ----------

func TestPrFormatAge_Days(t *testing.T) {
	ms := int64((2*24*60*60 + 3*60*60) * 1000) // 2d 3h
	got := prFormatAge(ms)
	want := "2d 3h"
	if got != want {
		t.Errorf("prFormatAge days: got %q want %q", got, want)
	}
}

func TestPrFormatAge_Hours(t *testing.T) {
	ms := int64((5*60*60 + 30*60) * 1000) // 5h 30m
	got := prFormatAge(ms)
	want := "5h 30m"
	if got != want {
		t.Errorf("prFormatAge hours: got %q want %q", got, want)
	}
}

func TestPrFormatAge_Minutes(t *testing.T) {
	ms := int64(45 * 60 * 1000) // 45m
	got := prFormatAge(ms)
	want := "45m"
	if got != want {
		t.Errorf("prFormatAge minutes: got %q want %q", got, want)
	}
}

func TestPrFormatAge_Seconds(t *testing.T) {
	ms := int64(30 * 1000) // 30s
	got := prFormatAge(ms)
	want := "30s"
	if got != want {
		t.Errorf("prFormatAge seconds: got %q want %q", got, want)
	}
}

func TestPrFormatAge_ZeroHoursInDays(t *testing.T) {
	// Exactly 1 day (24h); hours%24 == 0.
	ms := int64(24 * 60 * 60 * 1000)
	got := prFormatAge(ms)
	want := "1d 0h"
	if got != want {
		t.Errorf("prFormatAge 1d 0h: got %q want %q", got, want)
	}
}

// ---------- prRenderSummary ----------

func makePrTestPlan(completed bool) prPlan {
	phase1Status := "completed"
	phase2Status := "in_progress"
	if !completed {
		phase1Status = "in_progress"
	}
	return prPlan{
		ID:        "plan-abc-123",
		Title:     "Test Plan",
		Status:    "in_progress",
		CreatedAt: 0, // will compute age from nowMs
		Tags:      []string{"gpu", "rust"},
		LinkedFiles: []string{
			"/src/a.rs", "/src/b.rs", "/src/c.rs", "/src/d.rs",
			"/src/e.rs", "/src/f.rs", // 6 files → first 5 + "..."
		},
		Phases: []prPhase{
			{Number: 1, Title: "Phase One", Status: phase1Status},
			{Number: 2, Title: "Phase Two", Status: phase2Status},
		},
	}
}

func TestPrRenderSummary_ContainsBanner(t *testing.T) {
	plan := makePrTestPlan(true)
	nowMs := int64(5 * 24 * 60 * 60 * 1000) // plan is 5d old relative to nowMs=0
	out := prRenderSummary(plan, nowMs)

	checks := []string{
		"═══════════════════════════════════════════════════════════",
		"📋 ACTIVE PLAN: Test Plan",
		"Status: IN_PROGRESS",
		"Created: 5d 0h ago",
		"Progress: 1/2 phases (50%)",
		"Phases:",
		"✓ 1. Phase One",
		"⧗ 2. Phase Two",
		"Tags: gpu, rust",
		"Plan ID: plan-abc-123",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in summary:\n%s", want, out)
		}
	}
}

func TestPrRenderSummary_LinkedFilesCapped(t *testing.T) {
	plan := makePrTestPlan(true)
	nowMs := int64(60 * 1000)
	out := prRenderSummary(plan, nowMs)

	// 6 linked files → first 5 shown + "..."
	if !strings.Contains(out, "/src/e.rs") {
		t.Errorf("expected 5th file in output:\n%s", out)
	}
	if strings.Contains(out, "/src/f.rs") {
		t.Errorf("6th file should be truncated:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("expected '...' suffix for truncated files:\n%s", out)
	}
}

func TestPrRenderSummary_NoTags(t *testing.T) {
	plan := makePrTestPlan(true)
	plan.Tags = nil
	nowMs := int64(60 * 1000)
	out := prRenderSummary(plan, nowMs)

	if strings.Contains(out, "Tags:") {
		t.Errorf("Tags line should be absent when no tags:\n%s", out)
	}
}

func TestPrRenderSummary_NoFiles(t *testing.T) {
	plan := makePrTestPlan(true)
	plan.LinkedFiles = nil
	nowMs := int64(60 * 1000)
	out := prRenderSummary(plan, nowMs)

	if strings.Contains(out, "Files:") {
		t.Errorf("Files line should be absent when no linked files:\n%s", out)
	}
}

func TestPrRenderSummary_ProgressPercent(t *testing.T) {
	// 0 of 2 completed → 0%
	plan := makePrTestPlan(false)
	nowMs := int64(60 * 1000)
	out := prRenderSummary(plan, nowMs)
	if !strings.Contains(out, "0/2 phases (0%)") {
		t.Errorf("expected 0/2 phases (0%%) in output:\n%s", out)
	}
}

// ---------- cmdPlanResume integration test ----------

func TestCmdPlanResume_WritesFiles(t *testing.T) {
	dir := t.TempDir()

	// Override package vars to temp dir.
	origPlansDir := prPlansDir
	origSummary := prSummaryPath
	origLog := prLogPath
	prPlansDir = dir
	prSummaryPath = dir + "/plan-resume-summary.txt"
	prLogPath = dir + "/plan-resume.log"
	defer func() {
		prPlansDir = origPlansDir
		prSummaryPath = origSummary
		prLogPath = origLog
	}()

	// Write a plans-<uuid>.json with one in_progress and one completed plan.
	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	planJSON := `{
  "plans": [
    {
      "id": "plan-001",
      "title": "Active Plan",
      "status": "in_progress",
      "created_at": 0,
      "tags": ["test"],
      "linked_files": [],
      "phases": [
        {"number": 1, "title": "First Phase", "status": "completed"},
        {"number": 2, "title": "Second Phase", "status": "in_progress"}
      ]
    },
    {
      "id": "plan-002",
      "title": "Done Plan",
      "status": "completed",
      "created_at": 0,
      "tags": [],
      "linked_files": [],
      "phases": []
    }
  ]
}`
	planPath := dir + "/plans-" + convID + ".json"
	if err := os.WriteFile(planPath, []byte(planJSON), 0644); err != nil {
		t.Fatalf("write plans json: %v", err)
	}

	// Build stdin JSON pointing to a transcript path whose UUID matches convID.
	transcriptPath := "/home/user/.claude/projects/x/" + convID + ".jsonl"
	stdinJSON := `{"session_id":"sess-1","transcript_path":"` + transcriptPath + `"}`

	// Inject stdin.
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	_, _ = w.WriteString(stdinJSON)
	_ = w.Close()
	defer func() { os.Stdin = oldStdin }()

	cmdPlanResume()

	// Verify summary file was written.
	summaryData, err := os.ReadFile(prSummaryPath)
	if err != nil {
		t.Fatalf("summary file not written: %v", err)
	}
	summary := string(summaryData)
	if !strings.Contains(summary, "Active Plan") {
		t.Errorf("summary missing plan title:\n%s", summary)
	}
	if strings.Contains(summary, "Done Plan") {
		t.Errorf("completed plan should not appear in summary:\n%s", summary)
	}

	// Verify log file was written.
	logData, err := os.ReadFile(prLogPath)
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	logStr := string(logData)
	if !strings.Contains(logStr, `Resumed: "Active Plan"`) {
		t.Errorf("log missing resumed line:\n%s", logStr)
	}
	if !strings.Contains(logStr, "1/2 phases") {
		t.Errorf("log missing phase count:\n%s", logStr)
	}
}

func TestCmdPlanResume_NoPlansFile(t *testing.T) {
	dir := t.TempDir()

	origPlansDir := prPlansDir
	origSummary := prSummaryPath
	origLog := prLogPath
	prPlansDir = dir
	prSummaryPath = dir + "/plan-resume-summary.txt"
	prLogPath = dir + "/plan-resume.log"
	defer func() {
		prPlansDir = origPlansDir
		prSummaryPath = origSummary
		prLogPath = origLog
	}()

	convID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	transcriptPath := "/home/user/.claude/projects/x/" + convID + ".jsonl"
	stdinJSON := `{"session_id":"sess-1","transcript_path":"` + transcriptPath + `"}`

	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	_, _ = w.WriteString(stdinJSON)
	_ = w.Close()
	defer func() { os.Stdin = oldStdin }()

	cmdPlanResume() // should return silently, no crash

	if _, err := os.Stat(prSummaryPath); !os.IsNotExist(err) {
		t.Error("summary file should not exist when plans file is missing")
	}
}
