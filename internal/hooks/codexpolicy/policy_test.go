package codexpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPolicyIsValid(t *testing.T) {
	p := Default()
	if p.DefaultModel != "gpt-5.6-luna" || p.ReadOnlyModel != "gpt-5.6-luna" {
		t.Fatalf("unexpected cheap defaults: %+v", p)
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
		{"luna allowed", "orca-ide orchestration worker-start --task t --agent codex --model gpt-5.6-luna --json", false, ""},
		{"terra allowed", "orca-ide orchestration worker-start --agent=codex --model=gpt-5.6-terra", false, ""},
		{"sol requires opt in", "orca-ide orchestration worker-start --agent codex --model gpt-5.6-sol", true, "CODEX_ALLOW_SOL_WORKER=1"},
		{"sol explicitly allowed", "CODEX_ALLOW_SOL_WORKER=1 orca-ide orchestration worker-start --agent codex --model gpt-5.6-sol", false, ""},
		{"sol env explicitly allowed", "env CODEX_ALLOW_SOL_WORKER='1' orca-ide orchestration worker-start --agent codex --model gpt-5.6-sol", false, ""},
		{"missing model", "orca-ide orchestration worker-start --agent codex", true, "explicit --model"},
		{"unknown model", "/home/eric/.local/bin/orca-ide orchestration worker-start --agent codex --model mystery", true, "not in the host allowlist"},
		{"missing agent", "orca-ide orchestration worker-start --task t", true, "explicit --agent"},
		{"duplicate model", "orca-ide orchestration worker-start --agent codex --model gpt-5.6-luna --model gpt-5.6-terra", true, "duplicate --model"},
		{"other agent unaffected", "orca-ide orchestration worker-start --agent claude", false, ""},
		{"unrelated text unaffected", "echo orca-ide orchestration worker-start --agent codex", false, ""},
		{"chained unsafe launch", "true && orca orchestration worker-start --agent codex", true, "explicit --model"},
		{"environment prefix", "env TRACE=1 orca-ide orchestration worker-start --agent codex --model 'gpt-5.6-luna'", false, ""},
		{"single quoted semicolon in flag", "orca-ide orchestration worker-start --comment 'bounded; no writes' --agent codex --model gpt-5.6-terra", false, ""},
		{"double quoted semicolon in flag", "orca-ide orchestration worker-start --comment \"bounded; no writes\" --agent codex --model gpt-5.6-terra", false, ""},
		{"double quoted and in flag", "orca-ide orchestration worker-start --comment \"bounded && no writes\" --agent codex --model gpt-5.6-terra", false, ""},
		{"single quoted and in flag", "orca-ide orchestration worker-start --comment 'bounded && no writes' --agent codex --model gpt-5.6-terra", false, ""},
		{"single quoted or in flag", "orca-ide orchestration worker-start --comment 'bounded || no writes' --agent codex --model gpt-5.6-terra", false, ""},
		{"double quoted or in flag", "orca-ide orchestration worker-start --comment \"bounded || no writes\" --agent codex --model gpt-5.6-terra", false, ""},
		{"double quoted pipe in flag", "orca-ide orchestration worker-start --comment \"bounded | no writes\" --agent codex --model gpt-5.6-terra", false, ""},
		{"single quoted pipe in flag", "orca-ide orchestration worker-start --comment 'bounded | no writes' --agent codex --model gpt-5.6-terra", false, ""},
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
