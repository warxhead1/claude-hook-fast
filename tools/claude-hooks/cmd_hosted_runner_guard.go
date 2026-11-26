package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

// cmdHostedRunnerGuard refuses Write/Edit/MultiEdit of a GitHub workflow file
// whose new content selects a GitHub-hosted runner. CI runs only on our
// self-hosted runner. Fail-open on any parse problem. Block = exit 2.
func cmdHostedRunnerGuard() {
	input, ok := hookio.ReadHookInput()
	if !ok || len(input.ToolInput) == 0 {
		return
	}
	if os.Getenv("HOSTED_RUNNER_GUARD_DISABLE") == "1" {
		return
	}
	path, texts := hostedRunnerPayload(input.ToolName, input.ToolInput)
	if !isWorkflowPath(path) {
		return
	}
	for _, t := range texts {
		if label := hostedRunnerLabel(t); label != "" {
			fmt.Fprintf(os.Stderr, "WORKFLOW BLOCKED — `runs-on: %s` is a GitHub-hosted runner.\n\n"+
				"  File: %s\n\n"+
				"  CI runs ONLY on self-hosted runners (Eric ruling 2026-11-14): GitHub-hosted jobs are\n"+
				"  refused on billing and queue forever. Use `runs-on: self-hosted` (or a\n"+
				"  `[self-hosted, <label>]` list). If the job cannot run on our one runner, build it as a\n"+
				"  script plus a systemd user timer on the host.\n", label, path)
			hooklog.Writef("hosted-runner-guard: BLOCKED %s label=%q", path, label)
			hookExit(2)
		}
	}
}

func hostedRunnerPayload(tool string, raw json.RawMessage) (string, []string) {
	var ti struct {
		FilePath  string `json:"file_path"`
		Content   string `json:"content"`
		NewString string `json:"new_string"`
		Edits     []struct {
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(raw, &ti); err != nil {
		return "", nil
	}
	switch tool {
	case "Write":
		return ti.FilePath, []string{ti.Content}
	case "Edit":
		return ti.FilePath, []string{ti.NewString}
	case "MultiEdit":
		var out []string
		for _, e := range ti.Edits {
			out = append(out, e.NewString)
		}
		return ti.FilePath, out
	}
	return "", nil
}

func isWorkflowPath(p string) bool {
	if p == "" {
		return false
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if !strings.Contains(p, "/.github/workflows/") && !strings.HasPrefix(p, ".github/workflows/") {
		return false
	}
	ext := filepath.Ext(p)
	return ext == ".yml" || ext == ".yaml"
}

var (
	runsOnLine = regexp.MustCompile(`^\s*(?:-\s+)?runs-on:\s*(.*)$`)
	// ubuntu-latest, windows-latest, macos-14, ubuntu-22.04, windows-2022, any *-latest
	hostedLabel = regexp.MustCompile(`(?i)(?:^|[\s\[,"'])([a-z0-9._-]*-latest|macos-[a-z0-9.-]+|ubuntu-\d[\d.]*|windows-\d[\d.]*)(?:$|[\s\],"'])`)
)

// hostedRunnerLabel returns the first GitHub-hosted label on a runs-on line,
// or "". A line naming self-hosted passes.
func hostedRunnerLabel(content string) string {
	for _, line := range strings.Split(content, "\n") {
		m := runsOnLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := m[1]
		if i := strings.Index(val, " #"); i >= 0 {
			val = val[:i]
		}
		if strings.Contains(val, "self-hosted") {
			continue
		}
		if h := hostedLabel.FindStringSubmatch(val); h != nil {
			return h[1]
		}
	}
	return ""
}
