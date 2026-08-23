// cmd_kb_subagent_capture.go — SubagentStop hook: mirror
// cmd_kb_inbox_capture.go's touched-files -> `kb landmark` breadcrumb,
// but scoped to a DISPATCHED SUBAGENT's own effective cwd instead of the
// lead session's.
//
// Why this exists: cmd_kb_inbox_capture.go fires on the LEAD session's
// Stop event and reads `git status --porcelain` in the LEAD's cwd. A
// worktree-isolated subagent (isolation:"worktree", the sanctioned
// pattern per CLAUDE.md/worktrees.md) works in its OWN checkout — the
// lead's cwd never sees that diff, so today a subagent's file changes
// leave no kb breadcrumb at all unless the subagent itself remembers to
// write one.
//
// SubagentStop's hook payload carries `cwd` set to the subagent's own
// working directory (event.HookInput.EffectiveCWD()), and `subagent_type`
// naming which kind of agent it was — both absent from the lead's Stop
// payload. This hook uses exactly those two fields plus the shared
// touched-files/landmark helpers from cmd_kb_inbox_capture.go.
//
// Strictly ADVISORY and fail-open, same discipline as kb-inbox-capture:
// every error is swallowed, the normal SubagentStop ack is always
// emitted, and this must never be able to block or fail a dispatch.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

// kscMaxFiles caps how many touched-file paths get sent to `kb landmark
// --files` and how many appear in the title's path preview. Mirrors
// kicMaxFiles in cmd_kb_inbox_capture.go.
const kscMaxFiles = 15

// kscTitlePathPreviewCount bounds how many paths appear inline in the
// landmark title (kicMaxFiles still bounds --files itself) — the title
// is meant to be skimmable at `kb vet` time, not an exhaustive list.
const kscTitlePathPreviewCount = 3

// kscBuildLandmarkArgs constructs the argv for `kb landmark` given a
// subagent type, project, and the (already-capped) touched-file list.
// Title format: "Subagent <type>: <n> files touched — <first few paths>"
// so entries are filterable/skimmable at `kb vet` time. Extracted as a
// pure function for the same testability reason as kicBuildLandmarkArgs.
func kscBuildLandmarkArgs(subagentType, proj string, files []string) []string {
	if subagentType == "" {
		subagentType = "unknown"
	}
	preview := files
	if len(preview) > kscTitlePathPreviewCount {
		preview = preview[:kscTitlePathPreviewCount]
	}
	title := fmt.Sprintf("Subagent %s: %d files touched — %s",
		subagentType, len(files), strings.Join(preview, ", "))
	return []string{
		"landmark", title,
		"--project", proj,
		"--files", strings.Join(files, ","),
		"--status", "inbox",
		"--confidence", "0.3",
		"--json",
	}
}

// cmdKbSubagentCapture is the SubagentStop hook entry point. See package
// doc comment for full design/rationale.
func cmdKbSubagentCapture() {
	// Always emit the SubagentStop ack, even on early return — matches
	// cmdKbInboxCapture's discipline: this hook never uses
	// additionalContext (not part of the SubagentStop contract), it's a
	// silent breadcrumb, and it must never surface as a dispatch problem.
	defer func() {
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName": "SubagentStop",
			},
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(os.Stdout, string(b))
	}()

	in, _ := readStdinForHook()
	cwd := in.EffectiveCWD()

	proj, _ := project.Resolve(cwd)
	if proj == "" || proj == "unknown" {
		return
	}

	files := kicTouchedFiles(cwd) // shared with cmd_kb_inbox_capture.go
	if len(files) == 0 {
		return
	}
	if len(files) > kscMaxFiles {
		files = files[:kscMaxFiles]
	}

	binary := resolveKbBinary()
	if binary == "" {
		return
	}

	args := kscBuildLandmarkArgs(in.SubagentType, proj, files)
	kicRunLandmark(binary, args) // shared with cmd_kb_inbox_capture.go
}
