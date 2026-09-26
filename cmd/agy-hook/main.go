// agy-hook is a standalone CLI for Antigravity (agy) hook events. It
// publishes to nervous-bus and always exits 0 — no daemon dependency,
// telemetry must never break the agent. Go port of the Python prototype
// ~/.local/bin/agy-hook (wired from ~/.gemini/config/hooks.json).
package main

import (
	"os"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/timing"
)

func main() {
	start := time.Now()
	cmd := newAgyHookCmd()
	err := cmd.Execute()
	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	timing.Record("agy-hook", sub, time.Since(start))
	if err != nil {
		os.Exit(1)
	}
}
