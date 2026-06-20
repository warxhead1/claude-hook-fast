// cmd_plan_resume.go — Go port of session-start/plan-resume.js
//
// On SessionStart, loads /tmp/plans-<convId>.json for the current conversation
// and appends a human-readable plan summary to /tmp/plan-resume-summary.txt
// for each in_progress plan. Also logs to /tmp/plan-resume.log.
//
// Pure file I/O; NO stdout injection (matches JS behaviour: no console.log
// in the summary path, only fs.appendFileSync).
//
// FAF discipline: never panic, never block, never exit nonzero.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// prPlansDir is the directory that contains plans-<convId>.json files.
// Tests override this to a temp dir.
var prPlansDir = "/tmp"

// prSummaryPath is the file plans summaries are APPENDed to.
// Tests override this to a temp path.
var prSummaryPath = "/tmp/plan-resume-summary.txt"

// prLogPath is where plan-resume events are logged.
// Tests override this to a temp path.
var prLogPath = "/tmp/plan-resume.log"

// ---------- JSON shapes from the JS plan DB ----------

type prPhase struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type prPlan struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	CreatedAt   int64     `json:"created_at"` // epoch ms
	Tags        []string  `json:"tags"`
	LinkedFiles []string  `json:"linked_files"`
	Phases      []prPhase `json:"phases"`
}

type prPlanDB struct {
	Plans []prPlan `json:"plans"`
}

// ---------- age formatter (mirrors JS formatAge) ----------

// prFormatAge formats a duration in milliseconds as a human-readable age
// string, matching the JS logic exactly (floor division).
func prFormatAge(ms int64) string {
	secs := ms / 1000
	mins := secs / 60
	hours := mins / 60
	days := hours / 24

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours%24)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins%60)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// ---------- summary renderer (pure, testable) ----------

// prRenderSummary builds the ═══ banner string for a single plan.
// This is a pure function so tests can verify the output directly.
func prRenderSummary(plan prPlan, nowMs int64) string {
	totalPhases := len(plan.Phases)
	completed := 0
	for _, p := range plan.Phases {
		if p.Status == "completed" {
			completed++
		}
	}

	pct := 0
	if totalPhases > 0 {
		pct = int(math.Round(float64(completed) / float64(totalPhases) * 100))
	}

	age := nowMs - plan.CreatedAt
	ageStr := prFormatAge(age)

	var sb strings.Builder

	// Opening banner — matches JS template string lines 112-133.
	sb.WriteString("\n")
	sb.WriteString("═══════════════════════════════════════════════════════════\n")
	sb.WriteString(fmt.Sprintf("📋 ACTIVE PLAN: %s\n", plan.Title))
	sb.WriteString("═══════════════════════════════════════════════════════════\n")
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("Status: %s\n", strings.ToUpper(plan.Status)))
	sb.WriteString(fmt.Sprintf("Created: %s ago\n", ageStr))
	sb.WriteString(fmt.Sprintf("Progress: %d/%d phases (%d%%)\n", completed, totalPhases, pct))
	sb.WriteString("\n")
	sb.WriteString("Phases:\n")

	for _, p := range plan.Phases {
		icon := "○"
		switch p.Status {
		case "completed":
			icon = "✓"
		case "in_progress":
			icon = "⧗"
		}
		sb.WriteString(fmt.Sprintf("  %s %d. %s\n", icon, p.Number, p.Title))
	}

	sb.WriteString("\n")
	if len(plan.Tags) > 0 {
		sb.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(plan.Tags, ", ")))
	}
	if len(plan.LinkedFiles) > 0 {
		files := plan.LinkedFiles
		suffix := ""
		if len(files) > 5 {
			files = files[:5]
			suffix = "..."
		}
		sb.WriteString(fmt.Sprintf("Files: %s%s\n", strings.Join(files, ", "), suffix))
	}

	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("Plan ID: %s\n", plan.ID))
	sb.WriteString("═══════════════════════════════════════════════════════════\n")

	return sb.String()
}

// cmdPlanResume is the Go port of plan-resume.js.
func cmdPlanResume() {
	in, _ := readStdinForHook()
	transcriptPath := in.TranscriptPath

	convId := extractConversationUUID(transcriptPath)
	if convId == "" {
		return
	}

	// Load plans-<convId>.json.
	dbPath := filepath.Join(prPlansDir, "plans-"+convId+".json")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return
	}

	var db prPlanDB
	if err := json.Unmarshal(data, &db); err != nil {
		return
	}

	// Filter to in_progress plans.
	var activePlans []prPlan
	for _, p := range db.Plans {
		if p.Status == "in_progress" {
			activePlans = append(activePlans, p)
		}
	}
	if len(activePlans) == 0 {
		return
	}

	nowMs := time.Now().UnixMilli()

	for _, plan := range activePlans {
		summary := prRenderSummary(plan, nowMs)

		// Append to summary file (matching JS fs.appendFileSync).
		f, err := os.OpenFile(prSummaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			_, _ = f.WriteString(summary)
			_ = f.Close()
		}

		// Append to log file.
		totalPhases := len(plan.Phases)
		completed := 0
		for _, p := range plan.Phases {
			if p.Status == "completed" {
				completed++
			}
		}
		ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		logLine := fmt.Sprintf("%s - Resumed: %q (%d/%d phases)\n", ts, plan.Title, completed, totalPhases)
		lf, err := os.OpenFile(prLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			_, _ = lf.WriteString(logLine)
			_ = lf.Close()
		}
	}
}
