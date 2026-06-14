package main

import (
	"os"
	"strings"
	"testing"
)

// Validates the Go→Python resolver bridge: a raw "undeclared identifier" diagnostic
// for a known RES_ symbol is enriched with the exact `import <leaf_shard>;` fix.
// Skips gracefully if the reflex-tengine resolver isn't installed.
func TestReflexarcSlangFixes(t *testing.T) {
	home, _ := os.UserHomeDir()
	resolver := home + "/.config/nervous-bus/adapters/reflex-tengine/sentinels/slang_edit.py"
	if _, err := os.Stat(resolver); err != nil {
		t.Skip("reflex-tengine resolver not installed; skipping integration test")
	}
	stderr := "error: 'RES_PULSE_RECORD_RING' : undeclared identifier"
	got := reflexarcSlangFixes("foo.slang", stderr)
	if !strings.Contains(got, "import tengine_constants_debug;") {
		t.Fatalf("expected import fix in output, got: %q", got)
	}
}

func TestReflexarcSlangFixesEmptyStderr(t *testing.T) {
	if reflexarcSlangFixes("foo.slang", "") != "" {
		t.Fatal("empty stderr must yield no fixes")
	}
}
