package main

import (
	"encoding/json"
	"testing"
)

func TestShellCommandsSupportsCurrentAndLegacyFields(t *testing.T) {
	p := &hookPayload{ToolInput: json.RawMessage(`{"cmd":"orca-ide orchestration worker-start --agent codex"}`)}
	got := shellCommands(p)
	if len(got) != 1 || got[0] == "" {
		t.Fatalf("unexpected commands: %#v", got)
	}
	p = &hookPayload{Input: json.RawMessage(`{"command":["one","two"]}`)}
	got = shellCommands(p)
	if len(got) != 2 {
		t.Fatalf("unexpected legacy commands: %#v", got)
	}
}
