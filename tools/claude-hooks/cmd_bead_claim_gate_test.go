package main

// Tests for the bead-claim-gate family (cmd_bead_claim_gate.go).
//
// Two layers:
//   - Unit tests against the pure functions (detectBeadClaimEvents,
//     applyBeadClaimEvents, beadClaimGateDecision, load/save) — fast, no
//     subprocess.
//   - Harness tests (TestClaimGateHarness*) that build the real binary and
//     drive it over its actual JSON-stdin protocol, asserting the real
//     process exit code for both the warn (0) and block (2) rungs of the
//     ladder end-to-end.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// --- unit tests: detectBeadClaimEvents ---

func TestDetectBeadClaimEvents(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    []beadClaimEvent
	}{
		{
			name:    "claim-id-then-flag",
			command: "bd update tengine-afjfd --claim",
			want:    []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "claim-flag-then-id",
			command: "bd update --claim tengine-afjfd",
			want:    []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "update-without-claim-flag-is-not-a-claim",
			command: "bd update tengine-afjfd --priority=1",
			want:    nil,
		},
		{
			name:    "bd-close",
			command: "bd close tengine-afjfd",
			want:    []beadClaimEvent{{Kind: beadClaimEventBdClose, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "bd-comment",
			command: `bd comment tengine-afjfd "made progress, still working"`,
			want:    []beadClaimEvent{{Kind: beadClaimEventBdComment, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "verify-pending-close-via-python3",
			command: "python3 scripts/verify_pending.py close tengine-afjfd",
			want:    []beadClaimEvent{{Kind: beadClaimEventVerifyClose, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "verify-pending-parked-bare",
			command: "verify_pending.py parked tengine-afjfd",
			want:    []beadClaimEvent{{Kind: beadClaimEventVerifyParked, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "handback-escape",
			command: `echo UNVERIFIED-HANDBACK:tengine-afjfd "blocked by GPU driver bug, handing back"`,
			want:    []beadClaimEvent{{Kind: beadClaimEventHandback, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "chained-claim-then-unrelated",
			command: "bd update tengine-afjfd --claim && echo starting work",
			want:    []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "unrelated-command-no-events",
			command: "ls -la && git status --short",
			want:    nil,
		},
		{
			name:    "env-prefixed-bd-claim",
			command: "FOO=bar bd update tengine-afjfd --claim",
			want:    []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-afjfd"}},
		},
		{
			name:    "multiple-segments-claim-and-close-different-beads",
			command: "bd update tengine-aaaaa --claim; bd close tengine-bbbbb",
			want: []beadClaimEvent{
				{Kind: beadClaimEventClaim, BeadID: "tengine-aaaaa"},
				{Kind: beadClaimEventBdClose, BeadID: "tengine-bbbbb"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectBeadClaimEvents(tc.command)
			if len(got) != len(tc.want) {
				t.Fatalf("detectBeadClaimEvents(%q) = %+v, want %+v", tc.command, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- unit tests: applyBeadClaimEvents + beadClaimUndischargedIDs ---

func TestApplyBeadClaimEventsLifecycle(t *testing.T) {
	st := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{}}
	now := time.Now()

	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-a"}}, now, false)
	if got := beadClaimUndischargedIDs(st); len(got) != 1 || got[0] != "tengine-a" {
		t.Fatalf("after claim: undischarged = %v, want [tengine-a]", got)
	}

	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventBdClose, BeadID: "tengine-a"}}, now, false)
	if got := beadClaimUndischargedIDs(st); len(got) != 0 {
		t.Fatalf("after close: undischarged = %v, want []", got)
	}
	if !st.Claims["tengine-a"].Discharged || st.Claims["tengine-a"].DischargeReason != beadClaimEventBdClose {
		t.Fatalf("claim not marked discharged correctly: %+v", st.Claims["tengine-a"])
	}
}

func TestApplyBeadClaimEventsToolIsErrorSkipsRecording(t *testing.T) {
	st := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{}}
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-a"}}, time.Now(), true)
	if len(st.Claims) != 0 {
		t.Fatalf("toolIsError=true must not record events, got claims=%v", st.Claims)
	}
}

func TestApplyBeadClaimEventsDischargeOfUntrackedBeadIsNoop(t *testing.T) {
	st := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{}}
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventBdClose, BeadID: "tengine-never-claimed"}}, time.Now(), false)
	if len(st.Claims) != 0 {
		t.Fatalf("discharge of untracked bead must be a no-op, got claims=%v", st.Claims)
	}
}

func TestApplyBeadClaimEventsHandbackDischarges(t *testing.T) {
	st := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{}}
	now := time.Now()
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-a"}}, now, false)
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventHandback, BeadID: "tengine-a"}}, now, false)
	if !st.Claims["tengine-a"].Discharged || st.Claims["tengine-a"].DischargeReason != beadClaimEventHandback {
		t.Fatalf("handback must discharge: %+v", st.Claims["tengine-a"])
	}
}

// --- unit tests: beadClaimGateDecision ladder ---

func TestBeadClaimGateDecisionLadder(t *testing.T) {
	st := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{}}

	// No claims at all -> exit 0, silent.
	if code, msg := beadClaimGateDecision(st); code != 0 || msg != "" {
		t.Fatalf("empty state: got (%d, %q), want (0, \"\")", code, msg)
	}

	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-a"}}, time.Now(), false)

	// First Stop with an undischarged claim -> warn, exit 0.
	code, msg := beadClaimGateDecision(st)
	if code != 0 {
		t.Fatalf("rung1: got exit %d, want 0", code)
	}
	if msg == "" {
		t.Fatal("rung1: expected a warn message, got empty")
	}
	if !st.Warned {
		t.Fatal("rung1: expected Warned=true after first warn")
	}
	if !beadClaimSameIDs(st.WarnedIDs, []string{"tengine-a"}) {
		t.Fatalf("rung1: WarnedIDs = %v, want [tengine-a]", st.WarnedIDs)
	}

	// Second Stop, same undischarged set -> block, exit 2.
	code, msg = beadClaimGateDecision(st)
	if code != 2 {
		t.Fatalf("rung2: got exit %d, want 2", code)
	}
	if msg == "" {
		t.Fatal("rung2: expected a block message, got empty")
	}

	// Discharge it -> back to silent exit 0.
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventBdClose, BeadID: "tengine-a"}}, time.Now(), false)
	if code, msg := beadClaimGateDecision(st); code != 0 || msg != "" {
		t.Fatalf("after discharge: got (%d, %q), want (0, \"\")", code, msg)
	}
}

func TestBeadClaimGateDecisionResetsLadderOnChangedSet(t *testing.T) {
	st := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{}}
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-a"}}, time.Now(), false)

	if code, _ := beadClaimGateDecision(st); code != 0 {
		t.Fatalf("rung1 for tengine-a: got exit %d, want 0", code)
	}

	// A second, different claim shows up before the next Stop — the
	// undischarged set changed, so the ladder must warn again (rung 1),
	// not jump straight to block.
	applyBeadClaimEvents(st, []beadClaimEvent{{Kind: beadClaimEventClaim, BeadID: "tengine-b"}}, time.Now(), false)
	code, msg := beadClaimGateDecision(st)
	if code != 0 {
		t.Fatalf("changed set: got exit %d, want 0 (re-warn)", code)
	}
	if msg == "" {
		t.Fatal("changed set: expected a warn message")
	}
	want := []string{"tengine-a", "tengine-b"}
	sort.Strings(st.WarnedIDs)
	if !beadClaimSameIDs(st.WarnedIDs, want) {
		t.Fatalf("WarnedIDs = %v, want %v", st.WarnedIDs, want)
	}
}

// --- unit tests: state load/save fail-open ---

func TestLoadBeadClaimGateStateFailOpen(t *testing.T) {
	dir := t.TempDir()

	if _, ok := loadBeadClaimGateState(filepath.Join(dir, "does-not-exist.json")); ok {
		t.Fatal("missing file: expected ok=false")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadBeadClaimGateState(corrupt); ok {
		t.Fatal("corrupt file: expected ok=false")
	}

	valid := filepath.Join(dir, "valid.json")
	want := &beadClaimGateState{SessionID: "s1", Claims: map[string]*beadClaimRecord{
		"tengine-a": {BeadID: "tengine-a", ClaimedAt: "t0"},
	}}
	if err := saveBeadClaimGateState(valid, want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadBeadClaimGateState(valid)
	if !ok {
		t.Fatal("valid file: expected ok=true")
	}
	if got.SessionID != "s1" || got.Claims["tengine-a"].ClaimedAt != "t0" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestBeadClaimGateStatePath(t *testing.T) {
	orig := beadClaimGateStateDir
	defer func() { beadClaimGateStateDir = orig }()
	beadClaimGateStateDir = "/tmp"

	got := beadClaimGateStatePath("abc-123")
	want := "/tmp/claude-bead-claim-gate-abc-123.json"
	if got != want {
		t.Fatalf("beadClaimGateStatePath = %q, want %q", got, want)
	}
}

// --- harness: build the real binary and drive it over stdin JSON,
// asserting real process exit codes end-to-end. ---

var (
	beadClaimGateHarnessOnce sync.Once
	beadClaimGateHarnessBin  string
	beadClaimGateHarnessErr  error
)

// buildBeadClaimGateHarnessBinary compiles the claude-hook-fast binary once for
// the whole test binary run (not per-subtest, so t.TempDir()'s per-test
// cleanup can't remove it out from under a later test).
func buildBeadClaimGateHarnessBinary(t *testing.T) string {
	t.Helper()
	beadClaimGateHarnessOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bead-claim-gate-harness-bin-")
		if err != nil {
			beadClaimGateHarnessErr = err
			return
		}
		bin := filepath.Join(dir, "claude-hook-fast-test")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = mustGetwd(t)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			beadClaimGateHarnessErr = fmt.Errorf("go build: %v\n%s", err, stderr.String())
			return
		}
		beadClaimGateHarnessBin = bin
	})
	if beadClaimGateHarnessErr != nil {
		t.Fatalf("failed to build harness binary: %v", beadClaimGateHarnessErr)
	}
	return beadClaimGateHarnessBin
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// harnessHookInput mirrors the subset of the Claude Code hook stdin JSON
// protocol this feature consumes (see internal/hooks/event.HookInput).
type harnessHookInput struct {
	SessionID      string          `json:"session_id"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolIsError    bool            `json:"tool_is_error,omitempty"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
}

// runBeadClaimGateHarness invokes the built binary with the given subcommand,
// feeding stdin JSON, and returns (exitCode, stdout, stderr).
func runBeadClaimGateHarness(t *testing.T, bin, subcommand string, in harnessHookInput, home string) (int, string, string) {
	t.Helper()
	payload, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return runHarnessProcess(t, bin, []string{subcommand}, payload, append(os.Environ(), "HOME="+home))
}

// runHarnessProcess is the shared low-level driver: run bin with args,
// feed stdinPayload, use the given env, and return (exitCode, stdout,
// stderr). Shared by both the bead-claim-gate and the frozen claim-gate
// harness tests since they exercise the same compiled binary.
func runHarnessProcess(t *testing.T, bin string, args []string, stdinPayload []byte, env []string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(stdinPayload)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("subcommand %v failed to run: %v", args, err)
		}
	}
	return exitCode, stdout.String(), stderr.String()
}

func TestBeadClaimGateHarnessWarnThenBlock(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-warn-block-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	toolInput, _ := json.Marshal(map[string]string{"command": "bd update tengine-hzz01 --claim"})
	code, _, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID,
		ToolName:  "Bash",
		ToolInput: toolInput,
	}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate-track exit = %d, want 0", code)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected state file at %s: %v", statePath, err)
	}

	// First Stop: warn, exit 0.
	code, stdout, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("first bead-claim-gate exit = %d, want 0 (warn rung)", code)
	}
	if !bytes.Contains([]byte(stdout), []byte("systemMessage")) {
		t.Fatalf("first bead-claim-gate stdout missing systemMessage: %q", stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("tengine-hzz01")) {
		t.Fatalf("first bead-claim-gate stdout missing bead id: %q", stdout)
	}

	// Second Stop, same undischarged claim: block, exit 2.
	code, _, stderr := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 2 {
		t.Fatalf("second bead-claim-gate exit = %d, want 2 (block rung)", code)
	}
	if !bytes.Contains([]byte(stderr), []byte("tengine-hzz01")) {
		t.Fatalf("block stderr missing bead id: %q", stderr)
	}
}

func TestBeadClaimGateHarnessDischargeAvoidsBlock(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-discharge-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	claimInput, _ := json.Marshal(map[string]string{"command": "bd update tengine-hzz02 --claim"})
	runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash", ToolInput: claimInput,
	}, home)

	closeInput, _ := json.Marshal(map[string]string{"command": "bd close tengine-hzz02"})
	runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash", ToolInput: closeInput,
	}, home)

	code, stdout, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate exit = %d, want 0 (discharged)", code)
	}
	if stdout != "" {
		t.Fatalf("discharged session should be silent, got stdout=%q", stdout)
	}
}

func TestBeadClaimGateHarnessUnverifiedHandbackEscape(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-handback-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	claimInput, _ := json.Marshal(map[string]string{"command": "bd update tengine-hzz03 --claim"})
	runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash", ToolInput: claimInput,
	}, home)

	handbackInput, _ := json.Marshal(map[string]string{
		"command": `echo UNVERIFIED-HANDBACK:tengine-hzz03 "blocked, handing back"`,
	})
	runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash", ToolInput: handbackInput,
	}, home)

	code, _, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate exit = %d, want 0 (unverified-handback discharges)", code)
	}
}

func TestBeadClaimGateHarnessNoStateFileFailsOpen(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-no-state-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	_ = os.Remove(statePath) // ensure absent
	t.Cleanup(func() { _ = os.Remove(statePath) })

	code, stdout, stderr := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate exit = %d, want 0 (no state file)", code)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("expected silent exit, got stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestBeadClaimGateHarnessCorruptStateFailsOpen(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-corrupt-state-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	if err := os.WriteFile(statePath, []byte("{not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(statePath) })

	code, _, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate exit = %d, want 0 (corrupt state fails open)", code)
	}
}

func TestBeadClaimGateHarnessNonClaimingSessionNeverWritesState(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-non-claiming-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	_ = os.Remove(statePath)
	t.Cleanup(func() { _ = os.Remove(statePath) })

	unrelated, _ := json.Marshal(map[string]string{"command": "git status --short && ls -la"})
	code, _, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash", ToolInput: unrelated,
	}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate-track exit = %d, want 0", code)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatalf("non-claiming session must not create a state file at %s", statePath)
	}

	code, _, _ = runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate exit = %d, want 0 (never claimed)", code)
	}
}

func TestBeadClaimGateHarnessFailedClaimInvocationNotRecorded(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-failed-claim-01"
	statePath := "/tmp/claude-bead-claim-gate-" + sessionID + ".json"
	_ = os.Remove(statePath)
	t.Cleanup(func() { _ = os.Remove(statePath) })

	claimInput, _ := json.Marshal(map[string]string{"command": "bd update tengine-hzz04 --claim"})
	runBeadClaimGateHarness(t, bin, "bead-claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash", ToolInput: claimInput, ToolIsError: true,
	}, home)

	code, stdout, _ := runBeadClaimGateHarness(t, bin, "bead-claim-gate", harnessHookInput{SessionID: sessionID}, home)
	if code != 0 {
		t.Fatalf("bead-claim-gate exit = %d, want 0 (failed claim invocation never recorded)", code)
	}
	if stdout != "" {
		t.Fatalf("expected silent exit for never-recorded claim, got stdout=%q", stdout)
	}
}
