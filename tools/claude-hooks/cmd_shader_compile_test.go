package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeFakeResolver drops a deterministic stand-in for the reflex-tengine slang
// resolver and points REFLEX_SLANG_RESOLVER at it. The stub echoes a fixed
// `import` line only when the known symbol appears on stdin, so the test
// exercises the Go→resolver bridge (env override, stdin forwarding, arg
// passing, stdout trimming) without depending on the live tengine symbol index,
// whose contents change as that project evolves.
func writeFakeResolver(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping resolver-bridge test")
	}
	script := `import sys
stderr = sys.stdin.read()
if "RES_PULSE_RECORD_RING" in stderr:
    print("import tengine_constants_debug;")
`
	path := filepath.Join(t.TempDir(), "fake_slang_edit.py")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write fake resolver: %v", err)
	}
	t.Setenv("REFLEX_SLANG_RESOLVER", path)
}

// Validates the Go→Python resolver bridge: a raw "undeclared identifier"
// diagnostic for a known symbol is enriched with the exact `import` fix the
// resolver returns. Uses a controlled fake resolver, not the live one.
func TestReflexarcSlangFixes(t *testing.T) {
	writeFakeResolver(t)
	stderr := "error: 'RES_PULSE_RECORD_RING' : undeclared identifier"
	got := reflexarcSlangFixes("foo.slang", stderr)
	if got != "import tengine_constants_debug;" {
		t.Fatalf("expected the resolver's import fix (trimmed), got: %q", got)
	}
}

// A symbol the resolver does not recognize yields no fix — the bridge returns
// the resolver's empty output verbatim, never a fabricated import.
func TestReflexarcSlangFixesUnknownSymbol(t *testing.T) {
	writeFakeResolver(t)
	got := reflexarcSlangFixes("foo.slang", "error: 'RES_TOTALLY_UNKNOWN' : undeclared identifier")
	if got != "" {
		t.Fatalf("unknown symbol must yield no fix, got: %q", got)
	}
}

// A missing resolver is a graceful no-op, never an error or a panic.
func TestReflexarcSlangFixesResolverAbsent(t *testing.T) {
	t.Setenv("REFLEX_SLANG_RESOLVER", filepath.Join(t.TempDir(), "does-not-exist.py"))
	if got := reflexarcSlangFixes("foo.slang", "error: 'RES_PULSE_RECORD_RING' : undeclared identifier"); got != "" {
		t.Fatalf("absent resolver must yield no fix, got: %q", got)
	}
}

func TestReflexarcSlangFixesEmptyStderr(t *testing.T) {
	if reflexarcSlangFixes("foo.slang", "") != "" {
		t.Fatal("empty stderr must yield no fixes")
	}
}
