package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestKpsPrimeResult_Headline_PrefersRecent(t *testing.T) {
	r := kpsPrimeResult{
		Recent:      []kpsPrimeEntry{{Title: "Checkpoint: foo (2026-06-13)"}},
		ActivePlans: []kpsPrimeEntry{{Title: "should not win"}},
	}
	if got := r.headline(); got != "Checkpoint: foo (2026-06-13)" {
		t.Errorf("headline() = %q, want recent title", got)
	}
}

func TestKpsPrimeResult_Headline_FallsBackToActivePlans(t *testing.T) {
	r := kpsPrimeResult{
		ActivePlans: []kpsPrimeEntry{{Title: "Volcanic Reference Render Plan"}},
	}
	if got := r.headline(); got != "Volcanic Reference Render Plan" {
		t.Errorf("headline() = %q, want active_plans title", got)
	}
}

func TestKpsPrimeResult_Headline_FallsBackToGaps(t *testing.T) {
	r := kpsPrimeResult{
		Gaps: []kpsPrimeEntry{{Title: "SPIR-V Probe Injection Gap Analysis"}},
	}
	if got := r.headline(); got != "SPIR-V Probe Injection Gap Analysis" {
		t.Errorf("headline() = %q, want gaps title", got)
	}
}

func TestKpsPrimeResult_Headline_FallsBackToPendingVets(t *testing.T) {
	r := kpsPrimeResult{
		PendingVets: []string{"Session fluidity + KB curation design (2026-07-01)"},
	}
	if got := r.headline(); got != "Session fluidity + KB curation design (2026-07-01)" {
		t.Errorf("headline() = %q, want pending_vets[0]", got)
	}
}

func TestKpsPrimeResult_Headline_EmptyWhenNothing(t *testing.T) {
	if got := (kpsPrimeResult{}).headline(); got != "" {
		t.Errorf("headline() = %q, want empty", got)
	}
}

func TestKpsTruncateRunes_NoOpUnderLimit(t *testing.T) {
	if got := kpsTruncateRunes("short", 10); got != "short" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestKpsTruncateRunes_TruncatesRuneSafe(t *testing.T) {
	// 10 ASCII chars, cap at 5 -> 4 chars + ellipsis.
	got := kpsTruncateRunes("abcdefghij", 5)
	want := "abcd…"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestKpsRenderSiblingSection_SkipsEmptyHeadlines(t *testing.T) {
	headlines := map[string]string{
		"tengine": "Checkpoint: volcanic-reference-render (2026-06-13)",
		"hearth":  "", // nothing to report — must be omitted
	}
	order := []string{"tengine", "hearth"}
	got := kpsRenderSiblingSection(headlines, order)
	want := "## Sibling Activity\n- **tengine**: Checkpoint: volcanic-reference-render (2026-06-13)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestKpsRenderSiblingSection_EmptyWhenAllEmpty(t *testing.T) {
	headlines := map[string]string{"a": "", "b": ""}
	if got := kpsRenderSiblingSection(headlines, []string{"a", "b"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestKpsRenderSiblingSection_BoundedByCap(t *testing.T) {
	headlines := map[string]string{}
	order := []string{}
	// Build enough long headlines that the naive join would exceed the cap.
	for i := 0; i < 10; i++ {
		p := string(rune('a' + i))
		headlines[p] = "a fairly long headline that repeats itself to pad out the section length nicely"
		order = append(order, p)
	}
	got := kpsRenderSiblingSection(headlines, order)
	if len(got) > kpsSiblingSectionCapBytes {
		t.Errorf("rendered section len=%d exceeds cap=%d", len(got), kpsSiblingSectionCapBytes)
	}
	if got == "" {
		t.Fatal("expected non-empty section")
	}
}

func TestKpsSiblingProjects_FiltersStaleAndSameProject(t *testing.T) {
	fakeHome := t.TempDir()
	sessDir := filepath.Join(fakeHome, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", "/home/eric/projects")

	now := time.Now().UnixMilli()
	stale := time.Now().Add(-2 * time.Hour).UnixMilli()

	writeRec := func(name, cwd string, updatedAt int64) {
		data := []byte(`{"cwd":"` + cwd + `","updatedAt":` + strconv.FormatInt(updatedAt, 10) + `}`)
		if err := os.WriteFile(filepath.Join(sessDir, name+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeRec("live-sibling", "/home/eric/projects/tengine", now)
	writeRec("stale-sibling", "/home/eric/projects/nervous-bus", stale)
	writeRec("same-project", "/home/eric/projects/hearth", now)

	got := kpsSiblingProjects("hearth")
	if len(got) != 1 || got[0] != "tengine" {
		t.Errorf("kpsSiblingProjects() = %v, want [tengine]", got)
	}
}

func TestKpsSiblingProjects_CapsAtMax(t *testing.T) {
	fakeHome := t.TempDir()
	sessDir := filepath.Join(fakeHome, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", "/home/eric/projects")

	now := time.Now().UnixMilli()
	projects := []string{"tengine", "nervous-bus", "hearth-loom", "kb", "deer-flow"}
	for i, p := range projects {
		data := []byte(`{"cwd":"/home/eric/projects/` + p + `","updatedAt":` + strconv.FormatInt(now-int64(i), 10) + `}`)
		if err := os.WriteFile(filepath.Join(sessDir, p+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := kpsSiblingProjects("hearth")
	if len(got) != kpsMaxSiblings {
		t.Errorf("kpsSiblingProjects() len = %d, want %d (cap)", len(got), kpsMaxSiblings)
	}
}

func TestKpsSiblingProjects_MissingRegistryIsSilent(t *testing.T) {
	fakeHome := t.TempDir() // no .claude/sessions dir created
	t.Setenv("HOME", fakeHome)

	if got := kpsSiblingProjects("hearth"); got != nil {
		t.Errorf("kpsSiblingProjects() = %v, want nil for missing registry dir", got)
	}
}

func TestResolveKbBinary_PrefersDotLocalBin(t *testing.T) {
	fakeHome := t.TempDir()
	kbDir := filepath.Join(fakeHome, ".local", "bin")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kbBin := filepath.Join(kbDir, "kb")
	if err := os.WriteFile(kbBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	if got := resolveKbBinary(); got != kbBin {
		t.Errorf("resolveKbBinary() = %q, want %q", got, kbBin)
	}
}

func TestResolveKbBinary_MissingYieldsEmpty(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", fakeHome) // no kb anywhere reachable

	if got := resolveKbBinary(); got != "" {
		t.Errorf("resolveKbBinary() = %q, want empty", got)
	}
}
