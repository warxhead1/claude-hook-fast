package codexpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDefaultPolicyIsValid(t *testing.T) {
	p := Default()
	if p.DefaultModel != "gpt-6-astra" || p.ReadOnlyModel != "gpt-6.1-sol" {
		t.Fatalf("unexpected primary and read-only defaults: %+v", p)
	}
}

// TestManagedAgentsOnlyNameAllowedModels ensures the generated AGENTS.md text
// never recommends a model id outside the embedded policy's allowlist. This
// is the single source of truth check: a model dropped from (or never added
// to) worker-policy.json's allowed_worker_models cannot silently linger in
// the prose (the terra/luna regression this guards against, claude-hook-fast#6).
func TestManagedAgentsOnlyNameAllowedModels(t *testing.T) {
	p := Default()
	allowed := stringSet(p.AllowedWorkerModels)
	modelRE := regexp.MustCompile(`gpt-[0-9][A-Za-z0-9.\-]*`)
	text := string(ManagedAgentsBytes())
	for _, match := range modelRE.FindAllString(text, -1) {
		if !allowed[match] {
			t.Fatalf("AGENTS.md names model %q which is not in allowed_worker_models %v", match, p.AllowedWorkerModels)
		}
	}
	if !strings.Contains(text, "gpt-6-astra") {
		t.Fatal("AGENTS.md should still document the default/exceptional model")
	}
}

func TestInspectCommand(t *testing.T) {
	p := Default()
	tests := []struct {
		name    string
		command string
		blocked bool
		want    string
	}{
		{"sol allowed", "orca-ide orchestration worker-start --task t --agent codex --model gpt-6.1-sol --json", false, ""},
		{"previous sol rejected", "orca-ide orchestration worker-start --agent codex --model gpt-5.6-sol", true, "not in the host allowlist"},
		{"gpt-5.5 allowed", "orca-ide orchestration worker-start --agent=codex --model=gpt-5.5", false, ""},
		{"astra requires opt in", "orca-ide orchestration worker-start --agent codex --model gpt-6-astra", true, "CODEX_ALLOW_ASTRA_WORKER=1"},
		{"astra explicitly allowed", "CODEX_ALLOW_ASTRA_WORKER=1 orca-ide orchestration worker-start --agent codex --model gpt-6-astra", false, ""},
		{"astra env explicitly allowed", "env CODEX_ALLOW_ASTRA_WORKER='1' orca-ide orchestration worker-start --agent codex --model gpt-6-astra", false, ""},
		{"astra wrong opt in rejected", "CODEX_ALLOW_ASTRA_WORKER=0 orca-ide orchestration worker-start --agent codex --model gpt-6-astra", true, "CODEX_ALLOW_ASTRA_WORKER=1"},
		{"primary astra session unaffected", "codex --model gpt-6-astra", false, ""},
		{"terra rejected as dead model", "orca-ide orchestration worker-start --agent codex --model gpt-5.6-terra", true, "not in the host allowlist"},
		{"luna rejected as dead model", "orca-ide orchestration worker-start --agent codex --model gpt-5.6-luna", true, "not in the host allowlist"},
		{"missing model", "orca-ide orchestration worker-start --agent codex", true, "explicit --model"},
		{"unknown model", "/home/eric/.local/bin/orca-ide orchestration worker-start --agent codex --model mystery", true, "not in the host allowlist"},
		{"missing agent", "orca-ide orchestration worker-start --task t", true, "explicit --agent"},
		{"duplicate model", "orca-ide orchestration worker-start --agent codex --model gpt-6.1-sol --model gpt-5.5", true, "duplicate --model"},
		{"other agent unaffected", "orca-ide orchestration worker-start --agent claude", false, ""},
		{"unrelated text unaffected", "echo orca-ide orchestration worker-start --agent codex", false, ""},
		{"chained unsafe launch", "true && orca orchestration worker-start --agent codex", true, "explicit --model"},
		{"environment prefix", "env TRACE=1 orca-ide orchestration worker-start --agent codex --model 'gpt-6.1-sol'", false, ""},
		{"single quoted semicolon in flag", "orca-ide orchestration worker-start --comment 'bounded; no writes' --agent codex --model gpt-6.1-sol", false, ""},
		{"double quoted semicolon in flag", "orca-ide orchestration worker-start --comment \"bounded; no writes\" --agent codex --model gpt-6.1-sol", false, ""},
		{"double quoted and in flag", "orca-ide orchestration worker-start --comment \"bounded && no writes\" --agent codex --model gpt-6.1-sol", false, ""},
		{"single quoted and in flag", "orca-ide orchestration worker-start --comment 'bounded && no writes' --agent codex --model gpt-6.1-sol", false, ""},
		{"single quoted or in flag", "orca-ide orchestration worker-start --comment 'bounded || no writes' --agent codex --model gpt-6.1-sol", false, ""},
		{"double quoted or in flag", "orca-ide orchestration worker-start --comment \"bounded || no writes\" --agent codex --model gpt-6.1-sol", false, ""},
		{"double quoted pipe in flag", "orca-ide orchestration worker-start --comment \"bounded | no writes\" --agent codex --model gpt-6.1-sol", false, ""},
		{"single quoted pipe in flag", "orca-ide orchestration worker-start --comment 'bounded | no writes' --agent codex --model gpt-6.1-sol", false, ""},
		{"semicolon chained unsafe launch", "true; orca-ide orchestration worker-start --agent codex", true, "explicit --model"},
		{"and chained unsafe launch", "true && orca-ide orchestration worker-start --agent codex", true, "explicit --model"},
		{"or chained unsafe launch", "false || orca-ide orchestration worker-start --agent codex", true, "explicit --model"},
		{"pipe chained unsafe launch", "printf ok | orca-ide orchestration worker-start --agent codex", true, "explicit --model"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InspectCommand(tc.command, p)
			if (len(got) > 0) != tc.blocked {
				t.Fatalf("blocked=%v want %v: %+v", len(got) > 0, tc.blocked, got)
			}
			if tc.want != "" && !strings.Contains(got[0].Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", got[0].Reason, tc.want)
			}
		})
	}
}

func TestLoadPresentMalformedPolicyFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_WORKER_POLICY_PATH", path)
	if _, err := Load(); err == nil {
		t.Fatal("expected malformed installed policy to fail")
	}
}
