// cmd_kb_prime_siblings_live.go — "## Live Activity" tier for the
// SessionStart sibling-awareness hook (cmd_kb_prime_siblings.go).
//
// The existing "## Sibling Activity" section is sourced from kb's
// curated markdown vault — human-vetted, but can be stale by design
// (entries persist until re-vetted). This section is a DIFFERENT trust
// tier: a live read of nervous-bus's durable event log
// (~/.cache/nervous-bus/debug.jsonl, see internal/hooks/nbuspub/direct.go
// for the producer side) answering "is an orchestrator/loomie CURRENTLY
// running in a sibling project right now". It can be wrong or stale in
// the other direction — a crashed agent's last event still "looks live"
// until the freshness gate expires — so it gets:
//
//   - A much tighter staleness gate (kplaLiveFreshness) than the
//     curated section's kpsSiblingFreshness: minutes, not tens of
//     minutes. Silence (no line at all) beats a stale live-looking claim.
//   - Visually distinct rendering ("(live, as of HH:MM:SS)") so it never
//     gets confused with the curated headline for the same project.
//
// Read-only, best-effort, bounded: a fixed-size tail read plus JSON
// parsing, no subprocess. Any parse error on any line just skips that
// line — never fails the whole hook. The line->fact logic is a pure
// function (kplaExtractFact) so it's testable without touching the
// filesystem.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// kplaLiveFreshness is the staleness gate for "live" claims — much
	// tighter than kpsSiblingFreshness (30m) because this section is
	// claiming CURRENT activity, not "something happened recently".
	kplaLiveFreshness = 4 * time.Minute

	// kplaTailBytes bounds how much of debug.jsonl gets read from the
	// end — the file can grow large (rotated at debugLogMaxBytes ~500MB
	// in nbuspub), and we only care about the last few minutes of
	// events, so there is no reason to load the whole thing.
	kplaTailBytes = 512 * 1024

	// kplaTailLines caps how many trailing lines get parsed even if
	// kplaTailBytes captures more (defensive belt-and-suspenders against
	// pathologically short lines).
	kplaTailLines = 2000

	// kplaDescCapRunes bounds the rendered activity description per
	// line, matching kpsTitleCapRunes's rationale for the curated
	// section.
	kplaDescCapRunes = 100
)

// kplaTypePrefixes are the CloudEvents `type` prefixes this section
// treats as "an agent/orchestrator is doing something". See
// nervous-bus/schemas/bus.agent.activity.v1.json (host-level tool call/
// heartbeat stream) and loom.lifecycle*.v1.json (hearth-loom phase
// transitions) for the registered channel families.
var kplaTypePrefixes = []string{"bus.agent.activity.", "loom.lifecycle."}

// kplaEnvelope mirrors nbuspub.envelope (the producer's on-disk shape)
// for the fields this reader needs. Kept separate from nbuspub's
// internal type since that package doesn't export one.
type kplaEnvelope struct {
	Source string          `json:"source"`
	Type   string          `json:"type"`
	Time   string          `json:"time"`
	Data   json.RawMessage `json:"data"`
}

// kplaFact is one parsed "agent activity" data point extracted from a
// debug.jsonl line: which project it's about, when it happened, and a
// short human-readable description.
type kplaFact struct {
	project string
	ts      time.Time
	desc    string
}

// kplaHasTypePrefix reports whether typ starts with any registered
// live-activity channel prefix.
func kplaHasTypePrefix(typ string) bool {
	for _, p := range kplaTypePrefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

// kplaSourceProject extracts the project name from a CloudEvents
// `source` field. Convention (see nbuspub.SourceTag / event.PaneIDQualified
// callers): "/claude-host/<project>" for Claude Code host activity, or
// bare "/<project>" for other producers (e.g. "/hearth" for hearth-loom).
// Returns "" when no project-shaped component is found.
func kplaSourceProject(source string) string {
	source = strings.TrimPrefix(source, "/")
	parts := strings.Split(source, "/")
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		// "/claude-host/<project>" (or similar two-level source) -> last segment.
		return parts[len(parts)-1]
	}
}

// kplaSourceMatchesProject reports whether an envelope's source refers
// to proj — matching on either the bare-source form or the
// "/claude-host/<project>" form, per the task's matching contract.
func kplaSourceMatchesProject(source, proj string) bool {
	if source == "" || proj == "" {
		return false
	}
	return kplaSourceProject(source) == proj || strings.Contains(source, proj)
}

// kplaDescribe builds a short human-readable description of an
// activity event from its type and (best-effort) data payload. Falls
// back to the bare type when data doesn't carry a recognizable field —
// this must never fail on unexpected payload shapes.
func kplaDescribe(typ string, data json.RawMessage) string {
	var d map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &d) // best-effort; d stays nil on failure
	}
	switch {
	case strings.HasPrefix(typ, "bus.agent.activity."):
		event, _ := d["event"].(string)
		toolName, _ := d["tool_name"].(string)
		agentKind, _ := d["agent_kind"].(string)
		switch {
		case event != "" && toolName != "":
			return fmt.Sprintf("%s (%s)", event, toolName)
		case event != "":
			if agentKind != "" {
				return fmt.Sprintf("%s (%s)", event, agentKind)
			}
			return event
		}
		return "agent activity"
	case strings.HasPrefix(typ, "loom.lifecycle."):
		phase, _ := d["phase"].(string)
		status, _ := d["status"].(string)
		switch {
		case phase != "" && status != "":
			return fmt.Sprintf("%s: %s", phase, status)
		case phase != "":
			return phase
		case status != "":
			return status
		}
		return "loom lifecycle event"
	default:
		return typ
	}
}

// kplaExtractFact parses one debug.jsonl line into a kplaFact, or
// (kplaFact{}, false) when the line isn't a well-formed, in-scope
// activity event. Pure function — no I/O — so it's directly testable.
func kplaExtractFact(line []byte) (kplaFact, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return kplaFact{}, false
	}
	var env kplaEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return kplaFact{}, false
	}
	if !kplaHasTypePrefix(env.Type) {
		return kplaFact{}, false
	}
	ts, err := time.Parse(time.RFC3339, env.Time)
	if err != nil {
		// Producers may emit RFC3339Nano; retry before giving up.
		ts, err = time.Parse(time.RFC3339Nano, env.Time)
		if err != nil {
			return kplaFact{}, false
		}
	}
	proj := kplaSourceProject(env.Source)
	if proj == "" {
		return kplaFact{}, false
	}
	return kplaFact{
		project: proj,
		ts:      ts.UTC(),
		desc:    kplaDescribe(env.Type, env.Data),
	}, true
}

// kplaTailLinesOf reads up to the last maxBytes of path and returns its
// complete trailing lines (the first partial line after seeking into
// the middle of the file, if any, is dropped — correctness over
// completeness for a best-effort tail read).
func kplaTailLinesOf(path string, maxBytes int64) [][]byte {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}

	var lines [][]byte
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := start > 0
	for scanner.Scan() {
		if first {
			// Drop the first (possibly partial) line from a mid-file seek.
			first = false
			continue
		}
		b := make([]byte, len(scanner.Bytes()))
		copy(b, scanner.Bytes())
		lines = append(lines, b)
	}
	if len(lines) > kplaTailLines {
		lines = lines[len(lines)-kplaTailLines:]
	}
	return lines
}

// kplaDebugLogPath returns the debug.jsonl path, honoring
// $NERVOUS_DEBUG_LOG like nbuspub.appendDebugLog does.
func kplaDebugLogPath() string {
	if p := os.Getenv("NERVOUS_DEBUG_LOG"); p != "" {
		return p
	}
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "nervous-bus", "debug.jsonl")
}

// kplaLatestFacts scans the tail of debug.jsonl and returns, for each
// project in siblings, the single most recent in-scope fact within
// kplaLiveFreshness of now — or omits the project entirely when it has
// no fresh fact (silence, not a stale claim).
func kplaLatestFacts(lines [][]byte, siblings []string, now time.Time) map[string]kplaFact {
	want := make(map[string]bool, len(siblings))
	for _, s := range siblings {
		want[s] = true
	}
	cutoff := now.Add(-kplaLiveFreshness)

	best := make(map[string]kplaFact)
	for _, line := range lines {
		fact, ok := kplaExtractFact(line)
		if !ok || !want[fact.project] {
			continue
		}
		if fact.ts.Before(cutoff) {
			continue
		}
		if cur, exists := best[fact.project]; !exists || fact.ts.After(cur.ts) {
			best[fact.project] = fact
		}
	}
	return best
}

// kplaLiveActivity is the top-level best-effort entry point: reads the
// tail of debug.jsonl and returns the freshest live fact per sibling
// project. Every failure (missing file, unreadable, empty) yields an
// empty map — never an error, per the section's read-only/best-effort
// contract.
func kplaLiveActivity(siblings []string, now time.Time) map[string]kplaFact {
	path := kplaDebugLogPath()
	if path == "" || len(siblings) == 0 {
		return nil
	}
	lines := kplaTailLinesOf(path, kplaTailBytes)
	if len(lines) == 0 {
		return nil
	}
	return kplaLatestFacts(lines, siblings, now)
}

// kplaRenderLiveSection builds the "## Live Activity" block from a
// project->fact map, in the given display order. Returns "" when there's
// nothing fresh to report.
func kplaRenderLiveSection(facts map[string]kplaFact, order []string) string {
	var lines []string
	for _, p := range order {
		f, ok := facts[p]
		if !ok {
			continue
		}
		desc := kpsTruncateRunes(f.desc, kplaDescCapRunes)
		lines = append(lines, fmt.Sprintf("- **%s** (live, as of %s): %s",
			p, f.ts.Local().Format("15:04:05"), desc))
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Live Activity\n" + strings.Join(lines, "\n")
}
