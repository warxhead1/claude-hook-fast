package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

// cmdCleanupTmp ports ~/.claude/hooks/stop/cleanup-tmp.sh.
//
// Removes per-session /tmp files when a Claude Code session ends and kicks
// off a detached sweep of stale (>48h) files. Worst offender in the host's
// Stop-hook kill log: bash + jq + find each contribute fork+exec latency that
// cumulatively blows past the 2s budget under loomie load.
func cmdCleanupTmp() {
	input, _ := hookio.ReadHookInput()

	sessionID := ""
	convID := ""
	if input != nil {
		sessionID = input.SessionID
		if input.TranscriptPath != "" {
			convID = transcriptToConvID(input.TranscriptPath)
		}
	}

	for _, id := range []string{sessionID, convID} {
		if id == "" {
			continue
		}
		removeQuiet(fmt.Sprintf("/tmp/brain-hook-state-%s.json", id))
		removeQuiet(fmt.Sprintf("/tmp/hearth-ctx-%s.json", id))
		removeQuiet(fmt.Sprintf("/tmp/claude-context-cli_%s.json", id))

		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		matches, _ := filepath.Glob(fmt.Sprintf("/tmp/claude-context-cli_*_%s*.json", short))
		for _, m := range matches {
			removeQuiet(m)
		}
	}

	// Background sweep of files older than 48h. setsid escapes the
	// timeout-wrapper's process group so we don't block the hook on a slow
	// /tmp scan (the original bash version's whole reason for setsid).
	go func() {
		_ = exec.Command("setsid", "bash", "-c",
			`find /tmp -maxdepth 1 -name "brain-hook-state-*.json"   -mmin +2880 -delete 2>/dev/null
			 find /tmp -maxdepth 1 -name "claude-context-cli_*.json" -mmin +2880 -delete 2>/dev/null
			 find /tmp -maxdepth 1 -name "hearth-ctx-*.json"         -mmin +2880 -delete 2>/dev/null`,
		).Start()
		time.Sleep(10 * time.Millisecond) // let setsid detach before we exit
	}()

	os.Exit(0)
}

func transcriptToConvID(transcriptPath string) string {
	base := filepath.Base(transcriptPath)
	return trimSuffix(base, ".jsonl")
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

func removeQuiet(path string) {
	_ = os.Remove(path)
}
