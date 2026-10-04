package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func writeMarker(t *testing.T, root string, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := os.MkdirAll(wgFreezeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, freezeMarkerPath(root), string(b))
}

func withClaudePID(t *testing.T, pid int) {
	t.Helper()
	old := currentClaudePID
	currentClaudePID = func() int { return pid }
	t.Cleanup(func() { currentClaudePID = old })
}

func TestFreezeOwnerLine(t *testing.T) {
	newWgFixture(t)
	root := "/repo/x"
	live := os.Getpid()
	withClaudePID(t, live)

	cases := []struct {
		name    string
		marker  map[string]any
		session string
		sub     bool
		want    []string
		notWant []string
	}{
		{name: "same-session", marker: map[string]any{"session_id": "s1", "set_by": "dfg", "at": "T1", "description": "wave A"},
			session: "s1", want: []string{"YOU (this session, s1)", "set T1 by dfg for 'wave A'", "wave-freeze off /repo/x"}},
		{name: "subagent-of-owner", marker: map[string]any{"session_id": "s1", "set_by": "dfg", "at": "T1"},
			session: "s1", sub: true, want: []string{"YOUR PARENT", "escalate to it, do not lift"}},
		{name: "other-session", marker: map[string]any{"session_id": "s2", "set_by": "dfg", "at": "T1"},
			session: "s1", want: []string{"another session (s2)", "do not lift it"}, notWant: []string{"YOU"}},
		{name: "pid-only-match-after-compaction", marker: map[string]any{"session_id": "old", "claude_pid": live},
			session: "new", want: []string{"YOU", "same claude process", "old -> new"}},
		{name: "dead-pid-is-not-owner", marker: map[string]any{"session_id": "old", "claude_pid": 2147483000},
			session: "new", want: []string{"another session (old)"}},
		{name: "pid-differs", marker: map[string]any{"session_id": "old", "claude_pid": live + 1},
			session: "new", want: []string{"another session (old)"}},
		{name: "legacy-marker", marker: map[string]any{"repo": root},
			session: "s1", want: []string{"unknown (marker has no session_id)", freezeMarkerPath(root)}, notWant: []string{"YOU"}},
		{name: "orca-terminal-rendered", marker: map[string]any{"session_id": "s2", "orca_terminal_handle": "term_abc",
			"title": "my pane", "orca_worktree_path": "/wt/p"},
			session: "s1", want: []string{"Orca terminal: my pane [term_abc] in /wt/p"}},
		{name: "orca-title-absent", marker: map[string]any{"session_id": "s2", "orca_terminal_handle": "term_abc"},
			session: "s1", want: []string{"Orca terminal: (untitled) [term_abc]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeMarker(t, root, c.marker)
			got := freezeOwnerLine(root, c.session, c.sub)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in %q", w, got)
				}
			}
		})
	}
}

func TestFreezeOwnerLineMissingMarker(t *testing.T) {
	newWgFixture(t)
	if got := freezeOwnerLine("/repo/none", "s", false); !strings.Contains(got, "unknown") {
		t.Errorf("got %q", got)
	}
}

func TestSetFreezeRecordsOwnerFields(t *testing.T) {
	newWgFixture(t)
	t.Setenv("ORCA_TERMINAL_HANDLE", "term_h1")
	t.Setenv("ORCA_WORKTREE_ID", "wid")
	oldInfo := orcaTerminalInfo
	orcaTerminalInfo = func(h string) (string, string) {
		if h != "term_h1" {
			t.Errorf("handle %q", h)
		}
		return "pane title", "/wt/x"
	}
	t.Cleanup(func() { orcaTerminalInfo = oldInfo })
	withClaudePID(t, 4242)

	root := "/repo/y"
	if !setFreeze(root, "sess", "Agent", strings.Repeat("d", 300)) {
		t.Fatal("setFreeze failed")
	}
	m, ok := readFreezeMarker(root)
	if !ok {
		t.Fatal("marker unreadable")
	}
	if len(m.Description) != 120 || m.SessionID != "sess" || m.ClaudePID != 4242 ||
		m.OrcaTerminalHandle != "term_h1" || m.OrcaTerminalTitle != "pane title" || m.OrcaWorktreePath != "/wt/x" {
		t.Errorf("marker = %+v", m)
	}

	// orca-ide unavailable: handle recorded, title omitted, freeze still set.
	orcaTerminalInfo = func(string) (string, string) { return "", "" }
	if !setFreeze(root, "sess", "Agent", "") {
		t.Fatal("setFreeze failed")
	}
	m, _ = readFreezeMarker(root)
	if m.OrcaTerminalHandle != "term_h1" || m.OrcaTerminalTitle != "" {
		t.Errorf("marker = %+v", m)
	}
}

func TestFreezeOwnerLinesOnlyForWaveFreeze(t *testing.T) {
	newWgFixture(t)
	writeMarker(t, "/repo/z", map[string]any{"session_id": "s9"})
	got := freezeOwnerLines([]bashViolation{
		{Rule: "subagent", Root: "/repo/z"},
		{Rule: "wave-freeze", Root: "/repo/z"},
		{Rule: "wave-freeze", Root: "/repo/z"},
	}, "s1", false)
	n := 0
	for _, l := range got {
		if strings.HasPrefix(l, "Freeze owner:") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("owner lines = %d (%v)", n, got)
	}
	if len(freezeOwnerLines([]bashViolation{{Rule: "subagent", Root: "/repo/z"}}, "s1", true)) != 0 {
		t.Error("subagent-only violation should print no owner line")
	}
}
