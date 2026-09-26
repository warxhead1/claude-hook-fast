package main

// cmd_idle_stop_check.go — Stop-time mechanical check for "ended the turn
// with only a background waiter armed while other work was available".
// nervous-bus issue #36: the longest-running correction theme (~5 months,
// both harnesses) that the Inform-rung rule
// ~/.claude/memory-global/never-park-idle.md did not stop. This is the
// Automate-rung fix, built on the template that DID work for the parallel
// "monitor accuracy" theme: an Automate-rung hook check whose block message
// quotes the failure verbatim (see the monitor-unbounded-tail-for-single-
// outcome rule in ~/.claude/hook-rules.json).
//
// Subcommand: idle-stop-check (Stop event).
//
// Reads the transcript JSONL tail, isolates the assistant's tool_use calls
// issued since the LAST genuine user message (skipping tool_result replies
// and system-reminder/task-notification/slash-command injections — see
// idleStopIsGenuineUserBoundary), and fires (blocks the Stop once) when:
//
//  1. every tool_use this turn is a waiter-arming or read-only status
//     action (Bash with run_in_background=true running a sleep/wait-loop
//     command, Monitor, ScheduleWakeup, TaskOutput, Workflow, a background
//     Agent dispatch, or a read-only status probe such as `gh pr checks`,
//     `tail`/`cat` of a log, `git status`/`git log`), AND
//  2. at least one of those calls actually launched or is waiting on a
//     background thing (background Bash, background Agent, Workflow,
//     Monitor, ScheduleWakeup, TaskOutput) — a turn that only ran plain
//     status probes with nothing backgrounded does NOT fire, AND
//  3. the final assistant text contains no explicit blocked-on / nothing-
//     else-to-do statement.
//
// Fail-open everywhere: missing/empty transcript_path, any parse error, a
// turn boundary not found within the scan window, stop_hook_active, or a
// subagent transcript (hookio.IsSubagentTranscript) all skip silently —
// this hook must never brick a session. Kill switch:
// IDLE_STOP_CHECK_DISABLE=1.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

// idleStopScanBytes bounds how far back we read looking for the current
// turn's boundary. 2 MiB comfortably covers even a turn with many large
// tool results; if the boundary isn't found within this window we fail
// open (idleStopTurn.BoundaryFound stays false) rather than risk scanning
// a multi-hundred-MB transcript on every Stop.
const idleStopScanBytes = 2 * 1024 * 1024

// idleStopBlockMessage is the Stop-block reason. It quotes
// never-park-idle.md verbatim, per the template that stopped the parallel
// monitor-accuracy correction theme.
const idleStopBlockMessage = "A wait is never a turn's only activity. Name the independent work that can run now and start it — or state explicitly what you are blocked on and why nothing else can run.\n\n" +
	"Every status answer carries the clock: state elapsed time, the known cap or ETA, and what happens at the cap, read from the primary artifact in this same turn (ps -o etimes, the script's own timeout constant, the log's last line WITH its timestamp). \"Still waiting\" with no numbers is not a status."

// idleStopToolCall is the minimal shape needed to classify one tool_use
// block from the transcript.
type idleStopToolCall struct {
	Name            string
	RunInBackground bool
	Command         string
}

// idleStopTurn is everything extracted from the transcript tail about the
// assistant's activity since the last genuine user message.
type idleStopTurn struct {
	BoundaryFound bool
	ToolCalls     []idleStopToolCall
	FinalText     string
}

// transcriptEntry is the minimal JSONL line shape we need. Structural
// lines that carry no "message" (mode, bridge-session, cost-state,
// attachment, last-prompt, etc.) unmarshal with Message == nil and are
// skipped.
type transcriptEntry struct {
	Type    string `json:"type"`
	IsMeta  bool   `json:"isMeta"`
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock covers both assistant blocks (text, tool_use) and user
// blocks (text, tool_result, image) — we only read the fields relevant to
// each.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

var (
	// idleStopSegmentSplitRe splits a shell command into naive top-level
	// segments on &&, ||, ;, |, and newlines. Crude (no real shell
	// parsing), but necessary: an EARLIER unanchored version of the status
	// probe / sleep regexes below matched their target words as bare
	// substrings ANYWHERE in the command — including inside an unrelated
	// heredoc body. Replaying the classifier against real transcripts
	// (nervous-bus#36 PR) caught this concretely: a 1000+-char `python3 -
	// <<'EOF'` heredoc that happened to contain the literal text "git
	// diff" partway through its script body was misclassified as a
	// read-only status probe. Anchoring each check to a full top-level
	// segment closes that hole.
	idleStopSegmentSplitRe = regexp.MustCompile(`&&|\|\||[;|\n]`)

	// idleStopWaitLoopRe matches a full until/while ... do ... sleep ...
	// done poll loop anywhere in the command. Checked against the whole
	// command (not per-segment) because the loop's own `;`/newline
	// separators are part of its own shape; this pattern's four required
	// keywords in order make an accidental match inside unrelated script
	// content implausible.
	idleStopWaitLoopRe = regexp.MustCompile(`(?i)\b(until|while)\b[\s\S]*?\bdo\b[\s\S]*?\bsleep\b[\s\S]*?\bdone\b`)

	// idleStopBareSleepSegmentRe matches a segment that IS (starts with) a
	// sleep invocation, e.g. "sleep 30" as its own top-level command —
	// anchored so "sleep" appearing mid-word or mid-script doesn't count.
	idleStopBareSleepSegmentRe = regexp.MustCompile(`(?i)^sleep\s+\S`)

	// idleStopStatusSegmentRe matches a segment that IS (starts with) one
	// of the read-only status probes named in nervous-bus#36: gh pr
	// checks, tail/cat of a log, git status/log/diff. Anchored to segment
	// start — see idleStopSegmentSplitRe's comment for why.
	idleStopStatusSegmentRe = regexp.MustCompile(`(?i)^(gh\s+pr\s+checks|gh\s+pr\s+view|tail\b|cat\b|git\s+status|git\s+log|git\s+diff)\b`)

	// idleStopBenignSegmentRe matches connector segments that don't
	// themselves do anything read-sensitive (a leading `cd <path>` in a
	// chained command) — present without disqualifying an otherwise
	// all-status-probe command.
	idleStopBenignSegmentRe = regexp.MustCompile(`(?i)^cd\s+\S`)

	// idleStopBlockedPhraseRe matches an explicit blocked-on / nothing-else
	// statement in the assistant's final text — the escape hatch that
	// stops this from firing on a genuinely-blocked, correctly-reported
	// turn.
	idleStopBlockedPhraseRe = regexp.MustCompile(`(?i)(blocked on|nothing else|no independent work|nothing more to do|nothing else to do)`)

	// idleStopStripRes strips injected/synthetic wrapper tags from a user
	// message's text before judging whether any genuine human content
	// remains. Go's regexp (RE2) has no backreferences, so each tag pair
	// gets its own pattern rather than one parameterized one.
	idleStopStripRes = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
		regexp.MustCompile(`(?s)<task-notification>.*?</task-notification>`),
		regexp.MustCompile(`(?s)<local-command-caveat>.*?</local-command-caveat>`),
		regexp.MustCompile(`(?s)<command-name>.*?</command-name>`),
		regexp.MustCompile(`(?s)<command-message>.*?</command-message>`),
		regexp.MustCompile(`(?s)<command-args>.*?</command-args>`),
	}
)

// idleStopCommandSegments splits cmd into trimmed, non-empty top-level
// segments (see idleStopSegmentSplitRe).
func idleStopCommandSegments(cmd string) []string {
	parts := idleStopSegmentSplitRe.Split(cmd, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// idleStopIsWaiterBashCommand reports whether a background Bash command IS
// a wait: a full until/while ... sleep ... done loop, or a segment that is
// itself a bare sleep invocation.
func idleStopIsWaiterBashCommand(cmd string) bool {
	if idleStopWaitLoopRe.MatchString(cmd) {
		return true
	}
	for _, seg := range idleStopCommandSegments(cmd) {
		if idleStopBareSleepSegmentRe.MatchString(seg) {
			return true
		}
	}
	return false
}

// idleStopIsStatusProbeCommand reports whether EVERY top-level segment of
// cmd is either a read-only status probe or a benign `cd` connector — a
// chained command with even one non-probe segment (e.g. `git status && rm
// x`) is not read-only just because one segment matches.
func idleStopIsStatusProbeCommand(cmd string) bool {
	segs := idleStopCommandSegments(cmd)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if idleStopStatusSegmentRe.MatchString(seg) || idleStopBenignSegmentRe.MatchString(seg) {
			continue
		}
		return false
	}
	return true
}

// idleStopReadTail reads the last maxBytes of path (or the whole file if
// smaller). Mirrors internal/hooks/transcript's tail-read pattern.
func idleStopReadTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied transcript path, not user input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	offset := size - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size-offset)
	if _, err := io.ReadFull(f, buf); err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf, nil
}

// idleStopInputBool/idleStopInputString pull a single field out of a
// tool_use input blob without a full typed struct per tool.
func idleStopInputBool(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}

func idleStopInputString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// idleStopParseUserContent extracts plain text from a user message's
// content field, which is either a bare JSON string (a directly-typed
// prompt) or an array of content blocks (tool_result / text / image).
// onlyNonText is true when the content carries no text block at all
// (a pure tool_result or image reply) — that case is never a genuine
// turn boundary.
func idleStopParseUserContent(raw json.RawMessage) (text string, onlyNonText bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", true
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), len(parts) == 0
}

// idleStopStripInjectedTags removes system-reminder / task-notification /
// slash-command wrapper tags, returning whatever human-authored text (if
// any) remains.
func idleStopStripInjectedTags(s string) string {
	for _, re := range idleStopStripRes {
		s = re.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}

// idleStopIsGenuineUserBoundary reports whether a "user" transcript entry
// is a real human turn boundary rather than a tool_result reply or a
// synthetic injection (system-reminder, background task-notification,
// slash-command envelope). isMeta-flagged entries (Claude Code's own
// marker for local-command-caveat style synthetic messages) are excluded
// outright.
func idleStopIsGenuineUserBoundary(e transcriptEntry) bool {
	if e.IsMeta || e.Message == nil {
		return false
	}
	text, onlyNonText := idleStopParseUserContent(e.Message.Content)
	if onlyNonText {
		return false
	}
	// A background task-notification starts a NEW model turn: the harness
	// re-invokes the model with it. Without this, "notification arrives,
	// re-arm a waiter, stop" — the autonomous hold-loop shape of the
	// never-park-idle failure — would be merged into the earlier turn's
	// real work and never fire.
	if strings.Contains(text, "<task-notification>") {
		return true
	}
	return idleStopStripInjectedTags(text) != ""
}

// idleStopExtractTurn scans buf (a transcript tail) backward from EOF,
// stopping at the first genuine user boundary. It collects every
// assistant tool_use call issued after that boundary and the LAST
// assistant message's text (never falling back to an earlier assistant
// message's text if the final one had none — an empty final text is
// meaningful, not missing data).
func idleStopExtractTurn(buf []byte) idleStopTurn {
	lines := strings.Split(string(buf), "\n")
	var turn idleStopTurn
	seenLastAssistant := false

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e transcriptEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate malformed/truncated lines; keep scanning
		}
		if e.Message == nil {
			continue // structural line: mode, bridge-session, cost-state, ...
		}

		switch {
		case e.Type == "user" && e.Message.Role == "user":
			if idleStopIsGenuineUserBoundary(e) {
				turn.BoundaryFound = true
				return turn
			}
			// tool_result reply or injected reminder — not a boundary,
			// keep scanning backward.

		case e.Type == "assistant" && e.Message.Role == "assistant":
			var blocks []contentBlock
			if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
				continue
			}
			var textParts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_use":
					turn.ToolCalls = append(turn.ToolCalls, idleStopToolCall{
						Name:            b.Name,
						RunInBackground: idleStopInputBool(b.Input, "run_in_background"),
						Command:         idleStopInputString(b.Input, "command"),
					})
				}
			}
			if !seenLastAssistant {
				turn.FinalText = strings.Join(textParts, "\n")
				seenLastAssistant = true
			}
		}
	}
	// Ran off the front of the scan window without finding a boundary.
	turn.BoundaryFound = false
	return turn
}

// idleStopIsAllowedCall reports whether a single tool_use call belongs to
// the "waiter-arming or read-only status" set nervous-bus#36 names. Any
// call NOT in this set (Edit, Write, Read, Grep, a non-status Bash
// command, a foreground Agent dispatch, ...) disqualifies the turn from
// firing — it's real work, not idling.
func idleStopIsAllowedCall(tc idleStopToolCall) bool {
	switch tc.Name {
	case "Monitor", "ScheduleWakeup", "TaskOutput", "Workflow":
		return true
	case "Bash":
		if tc.RunInBackground && idleStopIsWaiterBashCommand(tc.Command) {
			return true
		}
		return idleStopIsStatusProbeCommand(tc.Command)
	case "Agent":
		// A synchronous (foreground) Agent dispatch is active work in
		// progress from the model's own turn, not an armed waiter; only a
		// background dispatch counts as "arm and walk away".
		return tc.RunInBackground
	}
	return false
}

// idleStopIsBackgroundSignal reports whether a call actually launched, or
// is waiting on, something backgrounded. This is the second required
// condition: a turn consisting ONLY of plain status probes (gh pr checks,
// git log) with nothing running in the background does not fire — there's
// no "waiter" to name, just an incomplete turn, which this check does not
// attempt to catch.
func idleStopIsBackgroundSignal(tc idleStopToolCall) bool {
	switch tc.Name {
	case "Monitor", "ScheduleWakeup", "TaskOutput", "Workflow":
		return true
	case "Bash", "Agent":
		return tc.RunInBackground
	}
	return false
}

// idleStopShouldFire applies the three-part nervous-bus#36 test to an
// extracted turn.
func idleStopShouldFire(turn idleStopTurn) bool {
	if !turn.BoundaryFound || len(turn.ToolCalls) == 0 {
		return false
	}
	backgroundSignal := false
	for _, tc := range turn.ToolCalls {
		if !idleStopIsAllowedCall(tc) {
			return false
		}
		if idleStopIsBackgroundSignal(tc) {
			backgroundSignal = true
		}
	}
	if !backgroundSignal {
		return false
	}
	if idleStopBlockedPhraseRe.MatchString(turn.FinalText) {
		return false
	}
	return true
}

// cmdIdleStopCheck is the "idle-stop-check" Stop subcommand.
func cmdIdleStopCheck() {
	if os.Getenv("IDLE_STOP_CHECK_DISABLE") == "1" {
		return
	}
	in, _ := readStdinForHook()
	if in.TranscriptPath == "" {
		return // can't verify top-level-ness or read a transcript: fail open
	}
	if in.StopHookActive {
		return // already mid stop-block cycle; never loop
	}
	// hookio's transcript-path check (matches /tasks/ and /subagents/) is
	// deliberately narrower than event.HookInput.IsSubagent's cwd-based
	// heuristic: this check must apply to top-level sessions dispatched
	// into an isolation worktree too (that cwd shape is exactly what
	// IsSubagent flags), so we key only on the transcript path shape
	// nervous-bus#36 asks for.
	if hookio.IsSubagentTranscript(in.TranscriptPath) {
		return
	}

	buf, err := idleStopReadTail(in.TranscriptPath, idleStopScanBytes)
	if err != nil {
		return // fail open: unreadable transcript
	}
	turn := idleStopExtractTurn(buf)
	if !idleStopShouldFire(turn) {
		return
	}

	hooklog.Writef("idle-stop-check: BLOCK session=%s transcript=%s tool_calls=%d", in.SessionID, in.TranscriptPath, len(turn.ToolCalls))
	out := map[string]any{
		"decision": "block",
		"reason":   idleStopBlockMessage,
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		return
	}
	fmt.Println(string(b))
}
