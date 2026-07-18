package main

// Tests for the frozen T6 claim-gate contract (cmd_claim_gate.go):
// edit-without-verify gate. Falsification cases per
// .planning/design_top10_2026_07_18.md section T6:
//
//   - session edits a .slang then Stops without verify -> flagged
//   - same session after `shadergen diagnose` -> passes
//   - docs-only session (.md edits only) -> never flagged
//
// Unit tests cover the pure classifier/whitelist/decision functions;
// harness tests build the real binary and drive it over the actual
// JSON-stdin protocol to prove exit codes end-to-end (0 warn / 2 block),
// including the WARN-mode default (repeat Stops stay exit 0, no ladder)
// and the BLOCK-mode config-flag opt-in.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- unit tests: claimGateIsGPUEffectful ---

func TestClaimGateIsGPUEffectful(t *testing.T) {
	cases := []struct {
		name string
		fp   string
		want bool
	}{
		{"slang-shader", "crates/tengine-dgc-hal/shaders/compute/cloud_baker.slang", true},
		{"glsl-shader", "crates/tengine-dgc-hal/shaders/includes/buffer_types.glsl", true},
		{"csrc-file", "crates/tengine-dgc-hal/csrc/ext_slang.c", true},
		{"dgc-hal-src-rust", "crates/tengine-dgc-hal/src/core.rs", true},
		{"generated-shader-excluded", "crates/tengine-dgc-hal/shaders/slang_generated/tengine_constants.slang", false},
		{"generated-dir-excluded", "crates/tengine-dgc-hal/shaders/generated/registry.glsl", false},
		{"docs-md-not-effectful", "docs/reference/WORK_TYPES.md", false},
		{"unrelated-rust-not-in-dgc-hal-src", "crates/tengine-director-python/src/lib.rs", false},
		{"empty-path", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimGateIsGPUEffectful(tc.fp); got != tc.want {
				t.Errorf("claimGateIsGPUEffectful(%q) = %v, want %v", tc.fp, got, tc.want)
			}
		})
	}
}

// --- unit tests: claimGateCommandIsVerifyWhitelisted ---

func TestClaimGateCommandIsVerifyWhitelisted(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		{"diagnose", "shadergen diagnose", true},
		{"readiness", "shadergen readiness", true},
		{"screenshot", "shadergen screenshot", true},
		{"silo-benchmark", "silo_tester race_full --benchmark --json", true},
		{"gpu-verify-lock", "bash scripts/gpu_verify_lock.sh race_full", true},
		{"pixel-diff", "python3 scripts/pixel_diff.py before.png after.png", true},
		{"render-metrics", "render_metrics.sh --silo race_full", true},
		{"step-then-read-combo", "shadergen step 60 && shadergen read CameraState.position", true},
		{"step-alone-insufficient", "shadergen step 60", false},
		{"read-alone-insufficient", "shadergen read CameraState.position", false},
		{"unrelated-command", "git status --short", false},
		{"silo-tester-without-benchmark", "silo_tester race_full --frames=200", false},
		{"prose-mention-not-invocation", `git commit -m "diagnose shadergen startup issue"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimGateCommandIsVerifyWhitelisted(tc.command); got != tc.want {
				t.Errorf("claimGateCommandIsVerifyWhitelisted(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

// --- unit tests: claimGateExtractHandbackReason ---

func TestClaimGateExtractHandbackReason(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"quoted-reason", `echo UNVERIFIED-HANDBACK:"blocked by GPU driver bug"`, "blocked by GPU driver bug"},
		{"bare-reason", "echo UNVERIFIED-HANDBACK:no-repro-on-this-hw", "no-repro-on-this-hw"},
		{"no-marker", "echo hello world", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimGateExtractHandbackReason(tc.command); got != tc.want {
				t.Errorf("claimGateExtractHandbackReason(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

// --- unit tests: claimGateDecision ---

func TestClaimGateDecisionNoEdit(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1"}
	if code, msg := claimGateDecision(st, "warn"); code != 0 || msg != "" {
		t.Fatalf("no edit: got (%d, %q), want (0, \"\")", code, msg)
	}
}

func TestClaimGateDecisionEditNoVerifyWarnMode(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1", LastEditTS: 100, LastEditFile: "x.slang"}
	code, msg := claimGateDecision(st, "warn")
	if code != 0 {
		t.Fatalf("warn mode: got exit %d, want 0", code)
	}
	if msg == "" {
		t.Fatal("warn mode: expected a WARN message")
	}
}

func TestClaimGateDecisionEditNoVerifyBlockMode(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1", LastEditTS: 100, LastEditFile: "x.slang"}
	code, msg := claimGateDecision(st, "block")
	if code != 2 {
		t.Fatalf("block mode: got exit %d, want 2", code)
	}
	if msg == "" {
		t.Fatal("block mode: expected a BLOCK message")
	}
}

func TestClaimGateDecisionVerifiedAfterEditPasses(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1", LastEditTS: 100, LastEditFile: "x.slang", LastVerifyTS: 200}
	if code, msg := claimGateDecision(st, "block"); code != 0 || msg != "" {
		t.Fatalf("verified after edit: got (%d, %q), want (0, \"\")", code, msg)
	}
}

func TestClaimGateDecisionStaleVerifyBeforeEditStillFlagged(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1", LastEditTS: 200, LastEditFile: "x.slang", LastVerifyTS: 100}
	code, msg := claimGateDecision(st, "warn")
	if code != 0 || msg == "" {
		t.Fatalf("stale verify (before edit): got (%d, %q), want (0, non-empty warn)", code, msg)
	}
}

func TestClaimGateDecisionHandbackAfterEditDischarges(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1", LastEditTS: 100, LastEditFile: "x.slang", HandbackTS: 200}
	if code, msg := claimGateDecision(st, "block"); code != 0 || msg != "" {
		t.Fatalf("handback after edit: got (%d, %q), want (0, \"\")", code, msg)
	}
}

func TestClaimGateDecisionHandbackBeforeEditStillFlagged(t *testing.T) {
	st := &editVerifyGateState{SessionID: "s1", LastEditTS: 200, LastEditFile: "x.slang", HandbackTS: 100}
	code, msg := claimGateDecision(st, "block")
	if code != 2 || msg == "" {
		t.Fatalf("stale handback (before edit): got (%d, %q), want (2, non-empty block)", code, msg)
	}
}

// --- unit tests: state load/save fail-open ---

func TestLoadEditVerifyGateStateFailOpen(t *testing.T) {
	dir := t.TempDir()

	if _, ok := loadEditVerifyGateState(filepath.Join(dir, "missing.json")); ok {
		t.Fatal("missing file: expected ok=false")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadEditVerifyGateState(corrupt); ok {
		t.Fatal("corrupt file: expected ok=false")
	}

	valid := filepath.Join(dir, "valid.json")
	want := &editVerifyGateState{SessionID: "s1", LastEditTS: 42, LastEditFile: "x.slang"}
	if err := saveEditVerifyGateState(valid, want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadEditVerifyGateState(valid)
	if !ok || got.LastEditTS != 42 || got.LastEditFile != "x.slang" {
		t.Fatalf("round-trip mismatch: ok=%v got=%+v", ok, got)
	}
}

func TestClaimGateStatePathFrozen(t *testing.T) {
	orig := claimGateStateDir
	defer func() { claimGateStateDir = orig }()
	claimGateStateDir = "/tmp"

	got := claimGateStatePath("abc-123")
	want := "/tmp/claude-claim-gate-abc-123.json"
	if got != want {
		t.Fatalf("claimGateStatePath = %q, want %q", got, want)
	}
}

func TestClaimGateModeDefaultsToWarn(t *testing.T) {
	t.Setenv("CLAIM_GATE_MODE", "")
	if got := claimGateMode(); got != "warn" {
		t.Fatalf("empty env: claimGateMode() = %q, want warn", got)
	}
	t.Setenv("CLAIM_GATE_MODE", "block")
	if got := claimGateMode(); got != "block" {
		t.Fatalf("block env: claimGateMode() = %q, want block", got)
	}
	t.Setenv("CLAIM_GATE_MODE", "BLOCK")
	if got := claimGateMode(); got != "block" {
		t.Fatalf("BLOCK (case-insensitive) env: claimGateMode() = %q, want block", got)
	}
	t.Setenv("CLAIM_GATE_MODE", "garbage")
	if got := claimGateMode(); got != "warn" {
		t.Fatalf("garbage env: claimGateMode() = %q, want warn (fail-safe)", got)
	}
}

// --- harness: build the real binary, drive the frozen falsification cases
// end-to-end over the real JSON-stdin protocol. Reuses the shared harness
// builder (buildBeadClaimGateHarnessBinary / runBeadClaimGateHarness) from
// cmd_bead_claim_gate_test.go — same binary, different subcommands.

func editToolInput(t *testing.T, filePath string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"file_path": filePath,
		"content":   "// edited",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bashToolInput(t *testing.T, command string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaimGateHarnessSlangEditThenStopWithoutVerifyFlagged(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-slang-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	code, _, _ := runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Edit",
		ToolInput: editToolInput(t, "crates/tengine-dgc-hal/shaders/compute/cloud_baker.slang"),
	}, home, "")
	if code != 0 {
		t.Fatalf("claim-gate-track exit = %d, want 0", code)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected state file at %s: %v", statePath, err)
	}

	// Falsification case 1: edit a .slang then Stop without verify -> flagged.
	code, stdout, _ := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "")
	if code != 0 {
		t.Fatalf("WARN mode: claim-gate exit = %d, want 0", code)
	}
	if !bytes.Contains([]byte(stdout), []byte("systemMessage")) {
		t.Fatalf("WARN mode: expected systemMessage flagging the edit, got stdout=%q", stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("cloud_baker.slang")) {
		t.Fatalf("WARN mode: expected flagged message to name the edited file, got %q", stdout)
	}

	// WARN mode has no ladder: a repeat Stop with the same unresolved
	// state must stay exit 0, not escalate to block.
	code, stdout2, _ := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "")
	if code != 0 {
		t.Fatalf("WARN mode repeat Stop: exit = %d, want 0 (no ladder)", code)
	}
	if !bytes.Contains([]byte(stdout2), []byte("systemMessage")) {
		t.Fatalf("WARN mode repeat Stop: expected another systemMessage, got %q", stdout2)
	}
}

func TestClaimGateHarnessBlockModeViaConfigFlag(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-block-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Write",
		ToolInput: editToolInput(t, "crates/tengine-dgc-hal/csrc/ext_slang.c"),
	}, home, "")

	// Opt-in BLOCK mode via the config flag.
	code, _, stderr := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "block")
	if code != 2 {
		t.Fatalf("BLOCK mode: claim-gate exit = %d, want 2", code)
	}
	if !bytes.Contains([]byte(stderr), []byte("ext_slang.c")) {
		t.Fatalf("BLOCK mode: expected stderr to name the edited file, got %q", stderr)
	}
}

func TestClaimGateHarnessSameSessionAfterDiagnosePasses(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-diagnose-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Edit",
		ToolInput: editToolInput(t, "crates/tengine-dgc-hal/shaders/compute/cloud_baker.slang"),
	}, home, "")

	// Give the verify timestamp room to sort strictly after the edit
	// timestamp (both are wall-clock UnixNano within the same process).
	time.Sleep(2 * time.Millisecond)

	// Falsification case 2: same session after `shadergen diagnose` -> passes.
	runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash",
		ToolInput: bashToolInput(t, "shadergen diagnose"),
	}, home, "")

	code, stdout, _ := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "block")
	if code != 0 {
		t.Fatalf("post-diagnose: claim-gate exit = %d, want 0", code)
	}
	if stdout != "" {
		t.Fatalf("post-diagnose: expected silent pass, got stdout=%q", stdout)
	}
}

func TestClaimGateHarnessDocsOnlySessionNeverFlagged(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-docs-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	_ = os.Remove(statePath)
	t.Cleanup(func() { _ = os.Remove(statePath) })

	// Falsification case 3: docs-only session (.md edits only) -> never flagged.
	code, _, _ := runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Edit",
		ToolInput: editToolInput(t, "docs/reference/WORK_TYPES.md"),
	}, home, "")
	if code != 0 {
		t.Fatalf("claim-gate-track exit = %d, want 0", code)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatalf("docs-only edit must not create a state file at %s", statePath)
	}

	code, stdout, _ := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "block")
	if code != 0 {
		t.Fatalf("docs-only session: claim-gate exit = %d, want 0", code)
	}
	if stdout != "" {
		t.Fatalf("docs-only session: expected silent pass, got stdout=%q", stdout)
	}
}

func TestClaimGateHarnessHandbackEscapeDischarges(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-handback-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	t.Cleanup(func() { _ = os.Remove(statePath) })

	runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Edit",
		ToolInput: editToolInput(t, "crates/tengine-dgc-hal/shaders/compute/cloud_baker.slang"),
	}, home, "")

	time.Sleep(2 * time.Millisecond)

	runClaimGateFrozenHarness(t, bin, "claim-gate-track", harnessHookInput{
		SessionID: sessionID, ToolName: "Bash",
		ToolInput: bashToolInput(t, `echo UNVERIFIED-HANDBACK:"cannot repro on this GPU"`),
	}, home, "")

	code, _, _ := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "block")
	if code != 0 {
		t.Fatalf("after handback: claim-gate exit = %d, want 0", code)
	}
}

func TestClaimGateHarnessNoStateFileFailsOpenFrozen(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-no-state-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	_ = os.Remove(statePath)
	t.Cleanup(func() { _ = os.Remove(statePath) })

	code, stdout, stderr := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "block")
	if code != 0 {
		t.Fatalf("no state file: exit = %d, want 0", code)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("no state file: expected silent, got stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestClaimGateHarnessCorruptStateFailsOpenFrozen(t *testing.T) {
	bin := buildBeadClaimGateHarnessBinary(t)
	home := t.TempDir()
	sessionID := "harness-frozen-corrupt-01"
	statePath := "/tmp/claude-claim-gate-" + sessionID + ".json"
	if err := os.WriteFile(statePath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(statePath) })

	code, _, _ := runClaimGateFrozenHarness(t, bin, "claim-gate", harnessHookInput{SessionID: sessionID}, home, "block")
	if code != 0 {
		t.Fatalf("corrupt state: exit = %d, want 0 (fail-open)", code)
	}
}

// runClaimGateFrozenHarness wraps runBeadClaimGateHarness with an optional
// CLAIM_GATE_MODE override (empty string leaves the default WARN mode).
func runClaimGateFrozenHarness(t *testing.T, bin, subcommand string, in harnessHookInput, home, mode string) (int, string, string) {
	t.Helper()
	payload, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	cmdArgs := []string{subcommand}
	env := append(os.Environ(), "HOME="+home)
	if mode != "" {
		env = append(env, "CLAIM_GATE_MODE="+mode)
	}
	return runHarnessProcess(t, bin, cmdArgs, payload, env)
}
