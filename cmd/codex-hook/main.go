// codex-hook is a standalone CLI for Codex CLI hook events.
// It publishes to nervous-bus and exits 0 — no daemon dependency.
package main

import (
	"os"
)

func main() {
	cmd := newCodexHookCmd()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}