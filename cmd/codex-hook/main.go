// codex-hook is a standalone CLI for Codex CLI hook events.
// It publishes to nervous-bus and exits 0 — no daemon dependency.
package main

import (
	"os"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/timing"
)

func main() {
	start := time.Now()
	cmd := newCodexHookCmd()
	err := cmd.Execute()
	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	timing.Record("codex-hook", sub, time.Since(start))
	if err != nil {
		os.Exit(1)
	}
}
