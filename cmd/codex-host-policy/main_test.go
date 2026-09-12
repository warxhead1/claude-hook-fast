package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderConfigSetsAstraDefaultAndAddsHook(t *testing.T) {
	in := []byte("# keep\nmodel = \"gpt-5.6-luna\"\nmodel_reasoning_effort = \"max\"\n[notice]\nhide = true\n")
	got, err := renderConfig(in, "gpt-6-astra", "/opt/bin/codex-hook")
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"# keep", `model = "gpt-6-astra"`, `model_reasoning_effort = "max"`, "hide = true", `command = "/opt/bin/codex-hook pre-tool"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	second, err := renderConfig(got, "gpt-6-astra", "/opt/bin/codex-hook")
	if err != nil || string(second) != text {
		t.Fatalf("render not idempotent: %v\n%s", err, second)
	}
}

func TestRenderConfigRecognizesExistingGuard(t *testing.T) {
	in := []byte(`model = "gpt-5.6-luna"
[[hooks.PreToolUse]]
matcher = "Bash|Read"
[[hooks.PreToolUse.hooks]]
type = "command"
command = "codex-hook pre-tool"
`)
	got, err := renderConfig(in, "gpt-5.6-luna", "/other/codex-hook")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "[[hooks.PreToolUse]]") != 1 {
		t.Fatalf("duplicated existing hook:\n%s", got)
	}
}

func TestRenderAgentsPreservesUnmanagedContentAndMigratesLegacy(t *testing.T) {
	policy := []byte("# Policy\nbody\n")
	got, err := renderAgents([]byte("personal rule\n"), policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "personal rule") || !strings.Contains(string(got), managedBegin) {
		t.Fatalf("unexpected managed content:\n%s", got)
	}
	second, err := renderAgents(got, []byte("# Policy\nnew body\n"))
	if err != nil || strings.Count(string(second), managedBegin) != 1 || !strings.Contains(string(second), "new body") {
		t.Fatalf("managed replacement failed: %v\n%s", err, second)
	}
	legacy, err := renderAgents([]byte("# Global model and delegation contract\nold\n"), policy)
	if err != nil || strings.Contains(string(legacy), "old") {
		t.Fatalf("legacy migration failed: %v\n%s", err, legacy)
	}
}

func TestInstallThenCheck(t *testing.T) {
	root := t.TempDir()
	opt := options{
		codexHome:  filepath.Join(root, "codex"),
		configHome: filepath.Join(root, "config"),
		hook:       "/opt/bin/codex-hook",
	}
	if err := reconcile(true, opt); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(false, opt); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(opt.codexHome, "config.toml"),
		filepath.Join(opt.codexHome, "AGENTS.md"),
		filepath.Join(opt.configHome, "claude-hook-fast", "codex-worker-policy.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}
