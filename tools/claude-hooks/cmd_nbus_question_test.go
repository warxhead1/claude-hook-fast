package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func runQuestionHook(t *testing.T, stdin string) map[string]any {
	t.Helper()
	get := capturePublish(t)
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, 0)
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = prev })
	cmdNbusPermissionRequest()
	var q []published
	for _, p := range get() {
		if p.channel == chanSessionQuestion {
			q = append(q, p)
		}
	}
	if len(q) != 1 {
		t.Fatalf("want 1 question publish, got %d", len(q))
	}
	return q[0].payload
}

func questionStdin(t *testing.T, cwd string) (string, []any) {
	t.Helper()
	qs := []any{
		map[string]any{"question": "Which db?", "header": "DB", "multiSelect": false,
			"options": []any{
				map[string]any{"label": "pg", "description": "Postgres"},
				map[string]any{"label": "sqlite", "description": "Embedded"},
			}},
		map[string]any{"question": "Which features?", "header": "Feat", "multiSelect": true,
			"options": []any{
				map[string]any{"label": "a", "description": "Alpha"},
				map[string]any{"label": "b", "description": "Beta"},
			}},
	}
	b, _ := json.Marshal(map[string]any{
		"session_id": "sess-q", "tool_use_id": "toolu_q1", "tool_name": "AskUserQuestion",
		"cwd": cwd, "tool_input": map[string]any{"questions": qs},
	})
	return string(b), qs
}

func TestAskUserQuestion_VerbatimQuestionsAndIdentity(t *testing.T) {
	t.Setenv("ORCA_TERMINAL_HANDLE", "")
	t.Setenv("ORCA_HISTFILE", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	stdin, qs := questionStdin(t, t.TempDir())
	p := runQuestionHook(t, stdin)

	if !reflect.DeepEqual(p["questions_all"], qs) {
		t.Errorf("questions_all not verbatim:\n got %v\nwant %v", p["questions_all"], qs)
	}
	if p["session_id"] != "sess-q" || p["tool_use_id"] != "toolu_q1" {
		t.Errorf("ids: %v %v", p["session_id"], p["tool_use_id"])
	}
	if p["question"] != "Which db?" || !reflect.DeepEqual(p["options"], []any{"pg", "sqlite"}) {
		t.Errorf("legacy fields changed: %v %v", p["question"], p["options"])
	}
	if p["correlation_id"] != "toolu_q1" {
		t.Errorf("correlation_id = %v", p["correlation_id"])
	}
	for _, k := range []string{"managed_by", "orca_terminal", "pane_id"} {
		if _, ok := p[k]; ok {
			t.Errorf("%s present outside Orca", k)
		}
	}
	t.Logf("payload: %s", mustJSON(p))
}

func TestAskUserQuestion_OrcaIdentity(t *testing.T) {
	t.Setenv("ORCA_TERMINAL_HANDLE", "term_abc")
	t.Setenv("ORCA_WORKTREE_ID", "wt-1")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	stdin, _ := questionStdin(t, t.TempDir())
	p := runQuestionHook(t, stdin)

	want := map[string]any{"managed_by": "orca", "orca_terminal": "term_abc",
		"orca_worktree_id": "wt-1", "pane_id": "orca:term_abc"}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}
	t.Logf("payload: %s", mustJSON(p))
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
