package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// plant puts secret at offset 0 and at offset 5000 of a filler body.
func plant(secret string) []string {
	filler := strings.Repeat("z", 6000)
	return []string{
		secret + filler,
		filler[:5000] + secret + filler[5000:],
	}
}

func TestActivityEnvelope_NeverCarriesContent(t *testing.T) {
	get := capturePublish(t)
	main, _ := repoFixture(t, "myrepo")
	target := filepath.Join(main, "pkg", "f.go")

	type tc struct {
		name   string
		tool   string
		input  func(body string) map[string]any
		resp   func(body string) any
		secret string
	}
	cases := []tc{
		{name: "write_body", tool: "Write", secret: "SECRET-WRITE-7f3a",
			input: func(b string) map[string]any { return map[string]any{"file_path": target, "content": b} }},
		{name: "edit_new_string", tool: "Edit", secret: "SECRET-EDITNEW-91bc",
			input: func(b string) map[string]any {
				return map[string]any{"file_path": target, "old_string": "a", "new_string": b}
			}},
		{name: "edit_old_string", tool: "Edit", secret: "SECRET-EDITOLD-55de",
			input: func(b string) map[string]any {
				return map[string]any{"file_path": target, "old_string": b, "new_string": "a"}
			}},
		{name: "multiedit", tool: "MultiEdit", secret: "SECRET-MULTI-2a2a",
			input: func(b string) map[string]any {
				return map[string]any{"file_path": target, "edits": []any{map[string]any{"old_string": b, "new_string": b}}}
			}},
		{name: "notebook", tool: "NotebookEdit", secret: "SECRET-NB-4c4c",
			input: func(b string) map[string]any {
				return map[string]any{"notebook_path": target, "new_source": b}
			}},
		{name: "read_response", tool: "Read", secret: "SECRET-READ-1d1d",
			input: func(string) map[string]any { return map[string]any{"file_path": target} },
			resp:  func(b string) any { return map[string]any{"file": map[string]any{"content": b}} }},
		{name: "bash_stdout", tool: "Bash", secret: "SECRET-STDOUT-8e8e",
			input: func(string) map[string]any { return map[string]any{"command": "cat x"} },
			resp:  func(b string) any { return map[string]any{"stdout": b, "stderr": ""} }},
		{name: "bash_env_assignment", tool: "Bash", secret: "SECRET-ENV-3b3b",
			input: func(b string) map[string]any {
				return map[string]any{"command": "API_KEY=" + b + " curl https://x.example/a"}
			}},
		{name: "bash_password_flag", tool: "Bash", secret: "SECRET-PW-6f6f",
			input: func(b string) map[string]any {
				return map[string]any{"command": "mysql -u root --password=" + b + " -e 'select 1'"}
			}},
		{name: "agent_prompt", tool: "Agent", secret: "SECRET-PROMPT-0a0a",
			input: func(b string) map[string]any {
				return map[string]any{"description": "d", "subagent_type": "Explore", "prompt": b}
			}},
		{name: "workflow_script", tool: "Workflow", secret: "SECRET-SCRIPT-9d9d",
			input: func(b string) map[string]any { return map[string]any{"script": b} }},
		{name: "grep_pattern", tool: "Grep", secret: "SECRET-GREP-7a7a",
			input: func(b string) map[string]any { return map[string]any{"pattern": b, "path": main} }},
		{name: "webfetch_query", tool: "WebFetch", secret: "SECRET-URL-5e5e",
			input: func(b string) map[string]any {
				return map[string]any{"url": "https://user:pw@host.example/p?token=" + b[:200] + "#" + b[len(b)-20:]}
			}},
		{name: "unknown_tool", tool: "mcp__x__y", secret: "SECRET-MCP-2b2b",
			input: func(b string) map[string]any { return map[string]any{"arg": b} }},
	}
	for _, c := range cases {
		for i, body := range func() []string {
			// Short-field cases need the secret inside the bounded slice.
			if c.name == "webfetch_query" {
				return []string{c.secret + strings.Repeat("z", 300), strings.Repeat("z", 100) + c.secret + strings.Repeat("z", 300)}
			}
			return plant(c.secret)
		}() {
			t.Run(c.name+"/"+[]string{"offset0", "offset5000"}[i], func(t *testing.T) {
				in := c.input(body)
				inRaw, _ := json.Marshal(in)
				var respRaw []byte
				payload := map[string]any{
					"session_id": "s", "cwd": main, "tool_name": c.tool, "tool_use_id": "tu",
					"tool_input": in,
				}
				if c.resp != nil {
					r := c.resp(body)
					payload["tool_response"] = r
					respRaw, _ = json.Marshal(r)
				}
				// Non-vacuous: the secret really is in what the hook receives.
				if !strings.Contains(string(inRaw)+string(respRaw), c.secret) {
					t.Fatalf("precondition: secret absent from tool input/response")
				}
				withStdin(t, string(toolInput(t, payload)), cmdNbusActivity)
				evs := get()
				if len(evs) == 0 {
					t.Fatalf("no event published")
				}
				for _, e := range evs {
					if strings.Contains(e.raw, c.secret) {
						t.Fatalf("secret reached the bus: %.400s", e.raw)
					}
				}
			})
		}
	}
}

func TestToolSummary_LargeWriteKeepsFilePath(t *testing.T) {
	path := "/home/eric/projects/x/" + strings.Repeat("d/", 40) + "file.go"
	raw, _ := json.Marshal(map[string]any{"file_path": path, "content": strings.Repeat("q", 100*1024)})
	var m map[string]any
	if err := json.Unmarshal([]byte(boundToolSummary("Write", raw)), &m); err != nil {
		t.Fatal(err)
	}
	if m["file_path"] != path || m["content_bytes"] != float64(100*1024) {
		t.Fatalf("summary = %v", m)
	}
}

func TestToolSummary_ShapesAndMetadata(t *testing.T) {
	parse := func(tool string, in map[string]any) map[string]any {
		raw, _ := json.Marshal(in)
		var m map[string]any
		if err := json.Unmarshal([]byte(boundToolSummary(tool, raw)), &m); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return m
	}
	m := parse("Edit", map[string]any{"file_path": "/a/b", "old_string": "abc", "new_string": "de"})
	if m["file_path"] != "/a/b" || m["old_string_bytes"] != float64(3) || m["new_string_bytes"] != float64(2) {
		t.Errorf("edit: %v", m)
	}
	m = parse("MultiEdit", map[string]any{"file_path": "/a/b", "edits": []any{
		map[string]any{"old_string": "a", "new_string": "bb"}, map[string]any{"old_string": "c", "new_string": "d"}}})
	if m["edits"] != float64(2) || m["new_string_bytes"] != float64(3) {
		t.Errorf("multiedit: %v", m)
	}
	m = parse("NotebookEdit", map[string]any{"notebook_path": "/n.ipynb", "new_source": "xyz"})
	if m["notebook_path"] != "/n.ipynb" || m["new_source_bytes"] != float64(3) {
		t.Errorf("notebook: %v", m)
	}
	m = parse("Agent", map[string]any{"description": "audit", "subagent_type": "Explore", "model": "haiku",
		"run_in_background": true, "prompt": "do things"})
	if m["description"] != "audit" || m["subagent_type"] != "Explore" || m["model"] != "haiku" ||
		m["run_in_background"] != true || m["prompt_bytes"] != float64(9) {
		t.Errorf("agent: %v", m)
	}
	m = parse("Bash", map[string]any{"command": "FOO=1 cd /x && go test ./... | tail -5; echo 'a; SECRET'"})
	if m["command"] != "cd go tail echo" {
		t.Errorf("bash verbs = %q", m["command"])
	}
}

func TestResponseSummary_SizeAndErrorOnly(t *testing.T) {
	var m map[string]any
	raw := json.RawMessage(`{"is_error":true,"content":"` + strings.Repeat("c", 400) + `"}`)
	if err := json.Unmarshal([]byte(boundedToolResponseSummary(raw)), &m); err != nil {
		t.Fatal(err)
	}
	if m["bytes"] != float64(len(raw)) || m["is_error"] != true || len(m) != 2 {
		t.Errorf("summary = %v", m)
	}
	for _, empty := range []string{"", "null", "{}", "[]"} {
		if got := boundedToolResponseSummary(json.RawMessage(empty)); got != "" {
			t.Errorf("%q -> %q, want empty", empty, got)
		}
	}
}

func TestExtractToolAction_WriteDetailHasNoContent(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"file_path": "/a/f.txt", "content": "SECRET-DETAIL"})
	_, detail := extractToolAction("Write", raw)
	if strings.Contains(detail, "SECRET-DETAIL") || detail != "/a/f.txt" {
		t.Errorf("detail = %q", detail)
	}
}
