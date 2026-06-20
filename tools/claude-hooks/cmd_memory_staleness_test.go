package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// ---------- msScan unit tests ----------

const msTestContent = `# Memory

## Undated Section
- This bullet has no date and should be skipped

## GPU Architecture (2024-01-10)
- Old bullet about GPU work
- CRITICAL: This critical thing is very stale
- Another stale bullet that goes over eighty characters in total so it gets truncated by the logic

## Recent Work (2099-12-31)
- This is a very recent entry and should not appear

## GPU Architecture (2025-06-15)
- Semi-old bullet that may or may not be stale depending on test date
`

func TestMsScan_StaleEntriesFound(t *testing.T) {
	// Use a fixed "now" far in the future so the 2024-01-10 section is always stale.
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	entries := msScan(msTestContent, now)

	// Should find entries from the 2024-01-10 section only (2099 is future, undated skipped).
	if len(entries) == 0 {
		t.Fatal("expected stale entries, got none")
	}

	// Verify all returned entries come from a dated section.
	for _, e := range entries {
		if e.section == "" {
			t.Errorf("entry has empty section: %+v", e)
		}
		if e.ageDays < 30 {
			t.Errorf("entry age %d < 30 days threshold: %+v", e.ageDays, e)
		}
	}
}

func TestMsScan_CriticalFlagged(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	entries := msScan(msTestContent, now)

	hasCritical := false
	for _, e := range entries {
		if e.critical {
			hasCritical = true
			if !strings.Contains(e.excerpt, "critical thing") {
				t.Errorf("critical entry excerpt unexpected: %q", e.excerpt)
			}
		}
	}
	if !hasCritical {
		t.Error("expected at least one critical entry")
	}
}

func TestMsScan_ExcerptTruncated(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	entries := msScan(msTestContent, now)

	foundTruncated := false
	for _, e := range entries {
		if strings.HasSuffix(e.excerpt, "...") {
			foundTruncated = true
			// Must be exactly 80 runes (77 + "...").
			runes := []rune(e.excerpt)
			if len(runes) != 80 {
				t.Errorf("truncated excerpt has %d runes, want 80", len(runes))
			}
		}
	}
	if !foundTruncated {
		t.Error("expected at least one truncated excerpt")
	}
}

func TestMsScan_UndatedSectionSkipped(t *testing.T) {
	content := `## No Date Here
- This should be skipped entirely
`
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	entries := msScan(content, now)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries from undated section, got %d", len(entries))
	}
}

func TestMsScan_FreshSectionSkipped(t *testing.T) {
	content := `## Recent (2099-12-31)
- This should not appear because the section date is in the future
`
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	entries := msScan(content, now)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries from fresh section, got %d", len(entries))
	}
}

func TestMsScan_EmptyContent(t *testing.T) {
	now := time.Now()
	entries := msScan("", now)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty content, got %d", len(entries))
	}
}

// ---------- cmdMemoryStaleness integration-style test ----------

func TestCmdMemoryStaleness_OutputBlock(t *testing.T) {
	// Write a temp MEMORY.md with known stale content.
	tmp := t.TempDir()
	memPath := tmp + "/MEMORY.md"

	content := `## Old Work (2020-01-01)
- stale bullet one
- CRITICAL: stale critical entry
- stale bullet two

## New Work (2099-01-01)
- fresh bullet
`
	if err := os.WriteFile(memPath, []byte(content), 0644); err != nil {
		t.Fatalf("write temp MEMORY.md: %v", err)
	}

	// Override package vars.
	orig := msMemoryPath
	msMemoryPath = memPath
	defer func() { msMemoryPath = orig }()

	origMax := os.Getenv("STALE_MEMORY_MAX")
	os.Setenv("STALE_MEMORY_MAX", "2")
	defer func() { os.Setenv("STALE_MEMORY_MAX", origMax) }()

	// Capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	cmdMemoryStaleness()

	_ = w.Close()
	os.Stdout = oldStdout

	var buf strings.Builder
	tmp2 := make([]byte, 8192)
	for {
		n, err := r.Read(tmp2)
		if n > 0 {
			buf.Write(tmp2[:n])
		}
		if err != nil {
			break
		}
	}
	_ = r.Close()

	out := buf.String()

	if !strings.Contains(out, "## STALE MEMORY WARNINGS (Tier A)") {
		t.Errorf("missing header in output:\n%s", out)
	}
	if !strings.Contains(out, "⚠️") {
		t.Errorf("expected critical warning emoji in output:\n%s", out)
	}
	if !strings.Contains(out, "| Section (date) | Age | Entry |") {
		t.Errorf("missing table header in output:\n%s", out)
	}
	// Count line: total should be 3 (2 stale + 1 critical), shown = 2 (cap), hidden = 1.
	if !strings.Contains(out, "and 1 more stale entries") {
		t.Errorf("expected hidden count line in output:\n%s", out)
	}
}

func TestCmdMemoryStaleness_MissingFile(t *testing.T) {
	orig := msMemoryPath
	msMemoryPath = "/tmp/does-not-exist-ms-test-9999.md"
	defer func() { msMemoryPath = orig }()

	// Capture stdout — should be empty.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	cmdMemoryStaleness()

	_ = w.Close()
	os.Stdout = oldStdout
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	_ = r.Close()

	if n != 0 {
		t.Errorf("expected no output for missing file, got %d bytes: %q", n, buf[:n])
	}
}
