package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRulesConfigPaths covers the two-layer discovery added 2026-07-11:
// user-global ~/.claude/hook-rules.json always applies; a project config
// layers on top instead of shadowing it.
func TestRulesConfigPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_HOOK_RULES_PATH", "")

	globalCfg := filepath.Join(home, ".claude", "hook-rules.json")
	proj := filepath.Join(home, "projects", "someproj")
	projCfg := filepath.Join(proj, ".claude", "hook-rules.json")
	deep := filepath.Join(proj, "crates", "x")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	writeCfg := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"version":1,"rules":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no configs anywhere -> empty", func(t *testing.T) {
		if got := rulesConfigPaths(deep); len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
	})

	writeCfg(globalCfg)

	t.Run("global only -> [global], found from any cwd", func(t *testing.T) {
		got := rulesConfigPaths(deep)
		if len(got) != 1 || got[0] != globalCfg {
			t.Errorf("want [%s], got %v", globalCfg, got)
		}
	})

	t.Run("global-only cwd walk-up dedupes (project discovery finds the global file)", func(t *testing.T) {
		got := rulesConfigPaths(filepath.Join(home, "somewhere"))
		if len(got) != 1 || got[0] != globalCfg {
			t.Errorf("want single deduped [%s], got %v", globalCfg, got)
		}
	})

	writeCfg(projCfg)

	t.Run("project config layers AFTER global, never shadows it", func(t *testing.T) {
		got := rulesConfigPaths(deep)
		if len(got) != 2 || got[0] != globalCfg || got[1] != projCfg {
			t.Errorf("want [global, project], got %v", got)
		}
	})

	t.Run("pinned path skips layering entirely", func(t *testing.T) {
		t.Setenv("CLAUDE_HOOK_RULES_PATH", projCfg)
		got := rulesConfigPaths(deep)
		if len(got) != 1 || got[0] != projCfg {
			t.Errorf("want pinned [%s], got %v", projCfg, got)
		}
	})
}

func TestCheckAgentNoIsolationReadOnlyExemption(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		ti   string
		want bool
	}{
		{"Explore without isolation -> exempt", `{"subagent_type":"Explore"}`, false},
		{"Plan without isolation -> exempt", `{"subagent_type":"Plan"}`, false},
		{"sonnet-worker without isolation -> block", `{"subagent_type":"sonnet-worker"}`, true},
		{"general-purpose without isolation -> block", `{"subagent_type":"general-purpose"}`, true},
		{"no subagent_type, no isolation -> block", `{"description":"x"}`, true},
		{"sonnet-worker WITH worktree -> pass", `{"subagent_type":"sonnet-worker","isolation":"worktree"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := checkAgentNoIsolation(repo, []byte(c.ti)); got != c.want {
				t.Errorf("checkAgentNoIsolation(repo, %q) = %v, want %v", c.ti, got, c.want)
			}
		})
	}
}

func TestBindingSpecPrefix(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"HERMES-H3-INVESTIGATE-DECIDE.md", "HERMES-H3"},
		{"HERMES-H3-INVESTIGATION-ACTIONS.md", "HERMES-H3"},
		{"LOOM-B1-DEADLETTER.md", "LOOM-B1"},
		{"README.md", ""},          // one token — no shared prefix inferable
		{"HERMES-H3.md", ""},       // two tokens — the whole name IS the prefix, no suffix to vary
		{"a-b-c.md", "a-b"},
	}
	for _, c := range cases {
		if got := bindingSpecPrefix(c.base); got != c.want {
			t.Errorf("bindingSpecPrefix(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestCheckDuplicateBindingSpec(t *testing.T) {
	tmp := t.TempDir()
	design := filepath.Join(tmp, "docs", "design")
	if err := os.MkdirAll(design, 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(design, "HERMES-H3-INVESTIGATE-DECIDE.md")
	if err := os.WriteFile(sibling, []byte("# Spec\n**Status:** BINDING build spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(design, "HERMES-H3-INVESTIGATION-ACTIONS.md")
	otherFeature := filepath.Join(design, "LOOM-B1-SOMETHING.md")

	mkInput := func(path, key, body string) string {
		b, _ := json.Marshal(body)
		p, _ := json.Marshal(path)
		return `{"file_path":` + string(p) + `,"` + key + `":` + string(b) + `}`
	}

	cases := []struct {
		name string
		ti   string
		want bool
	}{
		{"second BINDING spec, same prefix -> block", mkInput(target, "content", "Status: BINDING build spec"), true},
		{"Edit adding BINDING, same prefix -> block", mkInput(target, "new_string", "now BINDING"), true},
		{"different feature prefix -> pass", mkInput(otherFeature, "content", "Status: BINDING"), false},
		{"no BINDING in new content -> pass", mkInput(target, "content", "just notes"), false},
		{"outside docs/design -> pass", mkInput(filepath.Join(tmp, "docs", "HERMES-H3-X.md"), "content", "BINDING"), false},
		{"non-md file -> pass", mkInput(filepath.Join(design, "HERMES-H3-X.txt"), "content", "BINDING"), false},
		{"rewriting the binding doc itself -> pass (self excluded)", mkInput(sibling, "content", "Status: BINDING v2"), false},
		{"empty input -> fail-open", ``, false},
		{"malformed input -> fail-open", `nope`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := checkDuplicateBindingSpec([]byte(c.ti)); got != c.want {
				t.Errorf("checkDuplicateBindingSpec(%q) = %v, want %v", c.ti, got, c.want)
			}
		})
	}

	t.Run("superseded sibling unblocks", func(t *testing.T) {
		if err := os.WriteFile(sibling, []byte("**Status:** SUPERSEDED by HERMES-H3-INVESTIGATION-ACTIONS.md (was BINDING)\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if checkDuplicateBindingSpec([]byte(mkInput(target, "content", "Status: BINDING build spec"))) {
			t.Error("expected pass once the sibling is marked SUPERSEDED")
		}
	})
}

