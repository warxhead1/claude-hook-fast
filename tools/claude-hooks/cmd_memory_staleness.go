// cmd_memory_staleness.go — Go port of session-start/memory-staleness-check.js
//
// Scans a MEMORY.md for sections dated >= 30 days ago and emits a markdown
// staleness warning block to stdout so Claude Code injects it into session
// context.
//
// FAF discipline: never panic, never block, never exit nonzero.
// On any error → silent return (no stdout).
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// msMemoryPath is the default MEMORY.md location; tests override this var.
var msMemoryPath = "/home/eric/.claude/projects/-home-eric-projects-tengine/memory/MEMORY.md"

// msSectionDateRE matches "## Some Title (YYYY-MM-DD)" capturing the date.
var msSectionDateRE = regexp.MustCompile(`^##\s+.+\((\d{4}-\d{2}-\d{2})\)\s*$`)

// msSectionPrefixRE strips the leading "## " from a section header.
var msSectionPrefixRE = regexp.MustCompile(`^##\s+`)

// msEntry holds one stale bullet extracted from the MEMORY.md.
type msEntry struct {
	section  string
	date     string // "YYYY-MM-DD"
	ageDays  int
	excerpt  string
	critical bool
}

// msScan parses content and returns stale entries relative to now.
// Pure function — all I/O happens in cmdMemoryStaleness.
func msScan(content string, now time.Time) []msEntry {
	const staleDays = 30

	var (
		activeSectionDate time.Time
		activeSectionName string
		hasActiveDate     bool
		entries           []msEntry
	)

	for _, line := range strings.Split(content, "\n") {
		// Section header detection.
		if strings.HasPrefix(line, "## ") {
			m := msSectionDateRE.FindStringSubmatch(line)
			if m != nil {
				// Dated section.
				parsed, err := time.Parse("2006-01-02", m[1])
				if err == nil {
					activeSectionDate = parsed.UTC()
					activeSectionName = strings.TrimSpace(msSectionPrefixRE.ReplaceAllString(line, ""))
					hasActiveDate = true
				} else {
					hasActiveDate = false
					activeSectionName = ""
				}
			} else {
				// Undated section — clear.
				hasActiveDate = false
				activeSectionName = ""
			}
			continue
		}

		if !hasActiveDate {
			continue
		}

		isBullet := strings.HasPrefix(line, "- ")
		isCritical := strings.HasPrefix(line, "- CRITICAL:") || strings.HasPrefix(line, "CRITICAL:")
		if !isBullet && !isCritical {
			continue
		}

		// Age in whole days (floor), matching JS Math.floor((today - date) / 86400000).
		ageDays := int(now.Truncate(24*time.Hour).Sub(activeSectionDate.Truncate(24*time.Hour)).Hours() / 24)
		if ageDays < staleDays {
			continue
		}

		// Strip leading "- " and trim.
		raw := strings.TrimSpace(strings.TrimPrefix(line, "- "))

		// Excerpt: max 80 runes, truncate at 77 with "..." — rune-safe.
		excerpt := raw
		if utf8.RuneCountInString(raw) > 80 {
			runes := []rune(raw)
			excerpt = string(runes[:77]) + "..."
		}

		entries = append(entries, msEntry{
			section:  activeSectionName,
			date:     activeSectionDate.Format("2006-01-02"),
			ageDays:  ageDays,
			excerpt:  excerpt,
			critical: isCritical,
		})
	}

	return entries
}

// cmdMemoryStaleness is the Go port of memory-staleness-check.js.
func cmdMemoryStaleness() {
	content, err := os.ReadFile(msMemoryPath)
	if err != nil {
		// Missing or unreadable — silent exit, matching JS process.exit(0).
		return
	}

	now := time.Now()
	stale := msScan(string(content), now)
	if len(stale) == 0 {
		return
	}

	// Context budget cap: default 10, overrideable via STALE_MEMORY_MAX.
	maxRows := 10
	if v := os.Getenv("STALE_MEMORY_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxRows = n
		}
	}

	totalStale := len(stale)

	// Sort: critical-true first, then ageDays DESC (stable sort mirrors JS).
	sort.SliceStable(stale, func(i, j int) bool {
		ci, cj := stale[i].critical, stale[j].critical
		if ci != cj {
			return ci // true sorts before false
		}
		return stale[i].ageDays > stale[j].ageDays
	})

	shown := stale
	if len(shown) > maxRows {
		shown = shown[:maxRows]
	}
	hidden := totalStale - len(shown)

	// Build output lines (joined with "\n", final empty element = trailing newline).
	lines := []string{
		"## STALE MEMORY WARNINGS (Tier A)",
		"",
		fmt.Sprintf("%d MEMORY.md entries live in sections >= 30 days old (showing %d most relevant — CRITICAL + oldest). Verify against current code before citing:",
			totalStale, len(shown)),
		"",
		"| Section (date) | Age | Entry |",
		"|---|---|---|",
	}

	for _, e := range shown {
		// Escape pipes in excerpt to avoid breaking the markdown table.
		safe := strings.ReplaceAll(e.excerpt, "|", `\|`)
		crit := ""
		if e.critical {
			crit = "⚠️ "
		}
		lines = append(lines, fmt.Sprintf("| %s%s | %dd | %s |", crit, e.section, e.ageDays, safe))
	}

	if hidden > 0 {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("… and %d more stale entries — run `/memory` to review the full set.", hidden))
	}

	// Final empty string element produces a trailing newline when joined.
	lines = append(lines, "")

	_, _ = fmt.Fprint(os.Stdout, strings.Join(lines, "\n"))
}
