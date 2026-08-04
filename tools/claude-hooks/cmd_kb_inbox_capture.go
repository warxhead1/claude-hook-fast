// cmd_kb_inbox_capture.go — Stop hook: log a low-trust breadcrumb of files
// touched this session into kb's inbox (kb landmark --status inbox).
//
// Replaces the intent of the retired brain-session-summary.js (still on
// disk, unwired, at ~/.claude/hooks/stop/brain-session-summary.js — read
// only as reference, its behavior is NOT restored here) WITHOUT its
// failure modes:
//
//   - brain-session-summary.js fired on nearly every Stop, wrote
//     mechanical content (file basenames + a naive importance score)
//     into a single unscoped pool, and accumulated 29,394 near-zero-
//     signal entries that polluted every future query.
//   - This hook instead: (a) skips entirely when there's nothing to
//     report (no touched files, or project resolution fails), (b) writes
//     into kb's per-project `inbox` status — invisible to `kb prime`
//     until a human runs `kb vet <id>` — instead of a shared always-on
//     pool, and (c) makes no claim about WHAT happened, just which files
//     changed. Semantic summarization is explicitly out of scope.
//
// "Files touched this session" has no dedicated tracking left after
// brain's removal (its /tmp/brain-hook-state-*.json mechanism is gone
// along with it). We use `git status --porcelain` in cwd as a proxy:
// modified + untracked paths at Stop time. This is an intentionally
// imperfect signal — it misses files edited then reverted within the
// session, and any session with no working-tree cwd (e.g. a bare
// read-only run) yields nothing, which is fine: the skip-when-empty
// rule means those sessions correctly produce no capture.
//
// FAF discipline: every failure path swallows its error and returns.
// This must never be able to break a Stop event — hookSpecificOutput is
// always printed with just hookEventName (Stop hooks have no
// additionalContext surface; this is a silent breadcrumb for later
// `kb vet`/`kb files` lookups, not something the model sees).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

const (
	// kicGitStatusTimeout bounds the `git status --porcelain` call used
	// to approximate "files touched this session". Mirrors
	// checkCargoNoRustEditsTimeout's rationale in cmd_rules.go: an
	// unbounded git call can hang indefinitely on a contended
	// index.lock under heavy concurrent multi-agent/worktree load, and
	// Stop hooks have no SIGKILL-swallow net upstream of this binary.
	kicGitStatusTimeout = 2 * time.Second

	// kicLandmarkTimeout bounds the `kb landmark` shell-out.
	kicLandmarkTimeout = 3 * time.Second

	// kicMaxFiles caps how many touched-file paths get sent to
	// `kb landmark --files`, keeping the inbox entry (and the exec argv)
	// bounded regardless of how large a session's working tree diff is.
	kicMaxFiles = 15
)

// kicTouchedFiles runs `git -C cwd status --porcelain` (bounded, see
// kicGitStatusTimeout) and returns the list of modified/untracked paths.
// Any git failure (not a repo, timeout, missing binary) returns nil —
// callers treat that identically to "nothing touched" and skip.
func kicTouchedFiles(cwd string) []string {
	if cwd == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), kicGitStatusTimeout)
	defer cancel()
	out, err := osexec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain").Output() //nolint:gosec
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		// Porcelain format: "XY path" or "XY orig -> path" for renames.
		// Fields()[len-1] gives the (new) path in both cases.
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		files = append(files, parts[len(parts)-1])
	}
	return files
}

// kicBuildLandmarkArgs constructs the argv for `kb landmark` given a
// resolved project and the (already-capped) touched-file list. Extracted
// as a pure function so the flag-building logic is testable without an
// exec.
func kicBuildLandmarkArgs(proj string, files []string) []string {
	title := fmt.Sprintf("Session: %d files touched in %s", len(files), proj)
	return []string{
		"landmark", title,
		"--project", proj,
		"--files", strings.Join(files, ","),
		"--status", "inbox",
		"--confidence", "0.3",
		"--json",
	}
}

// kicRunLandmark shells out to `kb landmark` with the given args.
// Best-effort: errors are swallowed, nothing is returned or logged on
// failure (a broken kb binary must never surface as a Stop-hook problem).
func kicRunLandmark(binary string, args []string) {
	if binary == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), kicLandmarkTimeout)
	defer cancel()
	cmd := osexec.CommandContext(ctx, binary, args...) //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	_ = cmd.Run()
}

// cmdKbInboxCapture is the Stop hook entry point. See package doc
// comment for full design/rationale.
func cmdKbInboxCapture() {
	// Always emit the Stop ack, even on early return — Stop hooks don't
	// use additionalContext (never reaches the model), this is just a
	// well-formed no-op signal to the harness.
	defer func() {
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName": "Stop",
			},
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(os.Stdout, string(b))
	}()

	in, _ := readStdinForHook()
	cwd := in.EffectiveCWD()

	proj, _ := project.Resolve(cwd)
	if proj == "" || proj == "unknown" {
		// Don't pollute the vault with unattributed entries — matches
		// the "know what project this is" discipline brain lacked.
		return
	}

	files := kicTouchedFiles(cwd)
	if len(files) == 0 {
		// The critical discipline brain lacked: skip entirely rather
		// than record a session that (as far as this proxy can tell)
		// touched nothing.
		return
	}
	if len(files) > kicMaxFiles {
		files = files[:kicMaxFiles]
	}

	binary := resolveKbBinary()
	if binary == "" {
		return
	}

	args := kicBuildLandmarkArgs(proj, files)
	kicRunLandmark(binary, args)
}
