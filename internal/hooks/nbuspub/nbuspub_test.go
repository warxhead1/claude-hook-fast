package nbuspub

import (
	"strings"
	"testing"
)

func TestSourceTag(t *testing.T) {
	cases := map[string]string{
		"":            "/claude-host/unknown",
		"tengine":     "/claude-host/tengine",
		"hearth-loom": "/claude-host/hearth-loom",
	}
	for in, want := range cases {
		if got := SourceTag(in); got != want {
			t.Errorf("SourceTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChildEnv_StripsExistingNervousSource(t *testing.T) {
	osEnviron = func() []string {
		return []string{
			"HOME=/h",
			"NERVOUS_SOURCE=/old",
			"PATH=/usr/bin",
		}
	}
	defer func() { osEnviron = func() []string { return nil } }()

	env := childEnv("/claude-host/x")
	var saw int
	for _, kv := range env {
		if strings.HasPrefix(kv, "NERVOUS_SOURCE=") {
			saw++
			if kv != "NERVOUS_SOURCE=/claude-host/x" {
				t.Errorf("wrong override: %q", kv)
			}
		}
	}
	if saw != 1 {
		t.Errorf("expected exactly 1 NERVOUS_SOURCE entry, got %d", saw)
	}
	// NERVOUS_NO_ZELLIJ should have been added.
	found := false
	for _, kv := range env {
		if kv == "NERVOUS_NO_ZELLIJ=1" {
			found = true
		}
	}
	if !found {
		t.Error("expected NERVOUS_NO_ZELLIJ=1 in env")
	}
}

func TestPublish_SkipsWhenNervousMissing(t *testing.T) {
	// Empty PATH guarantees exec.LookPath fails.
	t.Setenv("PATH", "")
	err := Publish("bus.test.v1", "/claude-host/x", `{"k":"v"}`)
	if err != nil {
		t.Errorf("expected silent skip when nervous CLI missing, got %v", err)
	}
}
