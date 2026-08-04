package main

import (
	"testing"
	"time"
)

// ---------- kplaExtractFact ----------

func TestKplaExtractFact_AgentActivity(t *testing.T) {
	line := []byte(`{"specversion":"1.0","id":"x","source":"/claude-host/nervous-bus","type":"bus.agent.activity.v1","time":"2026-08-03T12:00:00Z","data":{"event":"tool_call","tool_name":"Edit","agent_kind":"host_claude_code"}}`)
	fact, ok := kplaExtractFact(line)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if fact.project != "nervous-bus" {
		t.Errorf("project = %q, want nervous-bus", fact.project)
	}
	if fact.desc != "tool_call (Edit)" {
		t.Errorf("desc = %q", fact.desc)
	}
}

func TestKplaExtractFact_LoomLifecycle(t *testing.T) {
	line := []byte(`{"source":"/hearth","type":"loom.lifecycle.phase.v1","time":"2026-08-03T12:00:00Z","data":{"phase":"execute","status":"running"}}`)
	fact, ok := kplaExtractFact(line)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if fact.project != "hearth" {
		t.Errorf("project = %q, want hearth", fact.project)
	}
	if fact.desc != "execute: running" {
		t.Errorf("desc = %q", fact.desc)
	}
}

func TestKplaExtractFact_IgnoresUnrelatedType(t *testing.T) {
	line := []byte(`{"source":"/hearth","type":"agent.session.v1","time":"2026-08-03T12:00:00Z","data":{}}`)
	if _, ok := kplaExtractFact(line); ok {
		t.Error("expected ok=false for non-matching type prefix")
	}
}

func TestKplaExtractFact_MalformedJSON(t *testing.T) {
	if _, ok := kplaExtractFact([]byte(`not json`)); ok {
		t.Error("expected ok=false for malformed json")
	}
}

func TestKplaExtractFact_EmptyLine(t *testing.T) {
	if _, ok := kplaExtractFact([]byte("")); ok {
		t.Error("expected ok=false for empty line")
	}
	if _, ok := kplaExtractFact([]byte("   ")); ok {
		t.Error("expected ok=false for whitespace-only line")
	}
}

func TestKplaExtractFact_BadTimestamp(t *testing.T) {
	line := []byte(`{"source":"/hearth","type":"loom.lifecycle.v1","time":"not-a-time","data":{}}`)
	if _, ok := kplaExtractFact(line); ok {
		t.Error("expected ok=false for unparsable timestamp")
	}
}

func TestKplaExtractFact_NoSourceProject(t *testing.T) {
	line := []byte(`{"source":"","type":"loom.lifecycle.v1","time":"2026-08-03T12:00:00Z","data":{}}`)
	if _, ok := kplaExtractFact(line); ok {
		t.Error("expected ok=false for empty source")
	}
}

// ---------- kplaSourceProject / kplaSourceMatchesProject ----------

func TestKplaSourceProject(t *testing.T) {
	cases := map[string]string{
		"/claude-host/nervous-bus": "nervous-bus",
		"/hearth":                  "hearth",
		"":                         "",
	}
	for in, want := range cases {
		if got := kplaSourceProject(in); got != want {
			t.Errorf("kplaSourceProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKplaSourceMatchesProject(t *testing.T) {
	if !kplaSourceMatchesProject("/claude-host/nervous-bus", "nervous-bus") {
		t.Error("expected match")
	}
	if kplaSourceMatchesProject("/claude-host/nervous-bus", "hearth") {
		t.Error("expected no match")
	}
	if kplaSourceMatchesProject("", "hearth") {
		t.Error("expected no match for empty source")
	}
}

// ---------- kplaLatestFacts (staleness gate + per-project best) ----------

func TestKplaLatestFacts_FiltersStale(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	lines := [][]byte{
		[]byte(`{"source":"/hearth","type":"loom.lifecycle.v1","time":"2026-08-03T11:50:00Z","data":{"phase":"stale"}}`), // 10m old, > 4m gate
		[]byte(`{"source":"/hearth","type":"loom.lifecycle.v1","time":"2026-08-03T11:58:00Z","data":{"phase":"fresh"}}`), // 2m old
	}
	facts := kplaLatestFacts(lines, []string{"hearth"}, now)
	f, ok := facts["hearth"]
	if !ok {
		t.Fatal("expected a fact for hearth")
	}
	if f.desc != "fresh" {
		t.Errorf("desc = %q, want fresh (stale entry should be filtered)", f.desc)
	}
}

func TestKplaLatestFacts_OnlyWantedProjects(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	lines := [][]byte{
		[]byte(`{"source":"/other-project","type":"loom.lifecycle.v1","time":"2026-08-03T11:59:00Z","data":{"phase":"x"}}`),
	}
	facts := kplaLatestFacts(lines, []string{"hearth"}, now)
	if len(facts) != 0 {
		t.Errorf("expected no facts for unrequested project, got %v", facts)
	}
}

func TestKplaLatestFacts_PicksMostRecent(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	lines := [][]byte{
		[]byte(`{"source":"/hearth","type":"loom.lifecycle.v1","time":"2026-08-03T11:57:00Z","data":{"phase":"older"}}`),
		[]byte(`{"source":"/hearth","type":"loom.lifecycle.v1","time":"2026-08-03T11:59:00Z","data":{"phase":"newer"}}`),
	}
	facts := kplaLatestFacts(lines, []string{"hearth"}, now)
	if facts["hearth"].desc != "newer" {
		t.Errorf("desc = %q, want newer (most recent should win)", facts["hearth"].desc)
	}
}

// ---------- kplaRenderLiveSection ----------

func TestKplaRenderLiveSection_Empty(t *testing.T) {
	if got := kplaRenderLiveSection(nil, []string{"hearth"}); got != "" {
		t.Errorf("expected empty section, got %q", got)
	}
}

func TestKplaRenderLiveSection_RendersInOrder(t *testing.T) {
	ts := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	facts := map[string]kplaFact{
		"hearth": {project: "hearth", ts: ts, desc: "execute: running"},
	}
	got := kplaRenderLiveSection(facts, []string{"hearth"})
	if got == "" {
		t.Fatal("expected non-empty section")
	}
	if got[:len("## Live Activity")] != "## Live Activity" {
		t.Errorf("missing header: %q", got)
	}
}
