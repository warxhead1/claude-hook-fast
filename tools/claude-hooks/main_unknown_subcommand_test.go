package main

import (
	"os"
	"strings"
	"testing"
)

// A settings.json entry that names a subcommand the installed binary lacks
// must not block the tool call (claude-hook-fast#50). On the previous main
// this exited 2, which a PreToolUse hook reads as BLOCK.
func TestUnknownSubcommand_FailsOpenAsHook(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	payload := []byte(`{"session_id":"s","tool_name":"Agent","tool_input":{"prompt":"x","model":"haiku"}}`)
	code, stdout, stderr := runHarnessProcess(t, bin, []string{"no-such-hook"}, payload,
		append(os.Environ(), "HOME="+t.TempDir()))
	if code != 0 {
		t.Fatalf("unknown subcommand as a hook exited %d; must fail open (0)", code)
	}
	if stdout != "" {
		t.Fatalf("unknown subcommand wrote stdout %q; a hook's stdout is parsed as a decision", stdout)
	}
	if !strings.Contains(stderr, `unknown subcommand "no-such-hook"`) {
		t.Fatalf("stderr should name the subcommand, got %q", stderr)
	}
}

func TestUnknownSubcommandExit_InteractiveStillErrors(t *testing.T) {
	if got := unknownSubcommandExit(true); got != 2 {
		t.Fatalf("interactive unknown subcommand = %d, want 2", got)
	}
	if got := unknownSubcommandExit(false); got != 0 {
		t.Fatalf("hook-context unknown subcommand = %d, want 0", got)
	}
}
