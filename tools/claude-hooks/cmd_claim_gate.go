package main

// cmd_claim_gate.go — Claim-gate: T6 frozen contract (edit-without-verify
// gate). See .planning/design_top10_2026_07_18.md section T6 in the
// tengine repo for the binding design. GPU-effectful edits (shaders,
// csrc/, tengine-dgc-hal src/) with no subsequent whitelisted verify
// invocation before the session Stops are flagged. This is a repo-local
// bonus companion to bead-claim-gate (cmd_bead_claim_gate.go, which covers
// the related-but-distinct undischarged-bd-claim problem) — do not
// confuse the two; this file owns the canonical `claim-gate`/
// `claim-gate-track` subcommand names per the freeze.
//
// Two subcommands:
//
//   - claim-gate-track (PostToolUse, Write|Edit|MultiEdit|Bash) —
//     accumulate-don't-scan. On a successful edit to a GPU-effectful file
//     (isShaderFile ∨ csrc/ ∨ tengine-dgc-hal src/, ¬isGeneratedShader),
//     records last_edit_ts. On a Bash command matching the verify
//     whitelist (shadergen diagnose|readiness|screenshot, a shadergen
//     step+read pair, silo_tester --benchmark, gpu_verify_lock.sh, or a
//     pixel-diff/render_metrics run), records last_verify_ts. The
//     UNVERIFIED-HANDBACK:<reason> escape is tracked independent of the
//     whitelist. State lives in /tmp/claude-claim-gate-<sessionID>.json.
//     Never reads the transcript.
//
//   - claim-gate (Stop) — O(1) read of that state file. Violation iff
//     last_edit_ts is set AND (no last_verify_ts, or it predates
//     last_edit_ts) AND no handback recorded at/after last_edit_ts. Mode
//     is WARN by default (systemMessage, exit 0) at launch; BLOCK (exit 2)
//     is opt-in via CLAIM_GATE_MODE=block, to be flipped only after
//     false-positive telemetry review. Unlike bead-claim-gate, there is NO
//     per-session warn-then-block ladder here — mode is a flat config
//     switch, not escalation by repeat Stop count.
//
// Fail-open everywhere: missing state file, corrupt JSON, missing
// session_id -> exit 0. A broken gate must never brick a session.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

// --- state file shape ---

type editVerifyGateState struct {
	SessionID      string `json:"session_id"`
	LastEditTS     int64  `json:"last_edit_ts,omitempty"` // UnixNano; 0 = unset
	LastEditFile   string `json:"last_edit_file,omitempty"`
	LastVerifyTS   int64  `json:"last_verify_ts,omitempty"` // UnixNano; 0 = unset
	LastVerifyCmd  string `json:"last_verify_cmd,omitempty"`
	HandbackReason string `json:"handback_reason,omitempty"`
	HandbackTS     int64  `json:"handback_ts,omitempty"` // UnixNano; 0 = unset
}

// claimGateStateDir is the directory holding the frozen gate's per-session
// state files. A package var (not an env var) so tests can redirect it,
// matching the prSummaryPath/prLogPath convention in cmd_plan_resume.go.
var claimGateStateDir = "/tmp"

func claimGateStatePath(sessionID string) string {
	return filepath.Join(claimGateStateDir, fmt.Sprintf("claude-claim-gate-%s.json", sessionID))
}

func loadEditVerifyGateState(path string) (*editVerifyGateState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var st editVerifyGateState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false
	}
	return &st, true
}

func saveEditVerifyGateState(path string, st *editVerifyGateState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// --- GPU-effectful classifier ---

// claimGateIsGPUEffectful reports whether an edit to fp should arm the
// gate: a shader source file, a csrc/ file, or a tengine-dgc-hal src/
// file — excluding anything under a generated-output directory.
// isShaderFile/isGeneratedShader are the existing classifiers from
// cmd_tengine_guards.go.
func claimGateIsGPUEffectful(fp string) bool {
	if fp == "" {
		return false
	}
	if isGeneratedShader(fp) {
		return false
	}
	if isShaderFile(fp) {
		return true
	}
	if strings.Contains(fp, "/csrc/") {
		return true
	}
	if strings.Contains(fp, "tengine-dgc-hal") && strings.Contains(fp, "/src/") {
		return true
	}
	return false
}

// --- verify whitelist ---

var (
	claimGateVerifyDiagnoseRe   = regexp.MustCompile(`\bshadergen\s+diagnose\b`)
	claimGateVerifyReadinessRe  = regexp.MustCompile(`\bshadergen\s+readiness\b`)
	claimGateVerifyScreenshotRe = regexp.MustCompile(`\bshadergen\s+screenshot\b`)
	claimGateVerifyStepRe       = regexp.MustCompile(`\bshadergen\s+step\b`)
	claimGateVerifyReadRe       = regexp.MustCompile(`\bshadergen\s+read\b`)
	claimGateVerifySiloBenchRe  = regexp.MustCompile(`\bsilo_tester\b[^&|;\n]*--benchmark\b`)
	claimGateVerifyGpuLockRe    = regexp.MustCompile(`\bgpu_verify_lock\.sh\b`)
	claimGateVerifyPixelDiffRe  = regexp.MustCompile(`\b(pixel[-_]?diff|render_metrics)(\.py|\.sh)?\b`)
)

// claimGateCommandIsVerifyWhitelisted reports whether a Bash command
// counts as a GPU verify step. shadergen step and shadergen read only
// count as verification together, in the same command (a step with no
// readback proves nothing).
func claimGateCommandIsVerifyWhitelisted(command string) bool {
	switch {
	case claimGateVerifyDiagnoseRe.MatchString(command),
		claimGateVerifyReadinessRe.MatchString(command),
		claimGateVerifyScreenshotRe.MatchString(command),
		claimGateVerifySiloBenchRe.MatchString(command),
		claimGateVerifyGpuLockRe.MatchString(command),
		claimGateVerifyPixelDiffRe.MatchString(command):
		return true
	}
	return claimGateVerifyStepRe.MatchString(command) && claimGateVerifyReadRe.MatchString(command)
}

var claimGateHandbackFullRe = regexp.MustCompile(`UNVERIFIED-HANDBACK:(\S.*)`)

// claimGateExtractHandbackReason returns the trimmed reason text after an
// UNVERIFIED-HANDBACK: marker, or "" if absent.
func claimGateExtractHandbackReason(command string) string {
	m := claimGateHandbackFullRe.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(strings.Trim(m[1], `"'`))
}

// --- edit-verify tool_input parsing ---

func claimGateEditFilePath(toolInput json.RawMessage) string {
	if len(toolInput) == 0 {
		return ""
	}
	var ti map[string]any
	if err := json.Unmarshal(toolInput, &ti); err != nil {
		return ""
	}
	fp, _ := ti["file_path"].(string)
	return fp
}

func claimGateBashCommand(toolInput json.RawMessage) string {
	if len(toolInput) == 0 {
		return ""
	}
	var ti struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(toolInput, &ti); err != nil {
		return ""
	}
	return ti.Command
}

// --- track (PostToolUse) ---

func claimGateTrackEdit(in event.HookInput) {
	if in.ToolIsError {
		return
	}
	fp := claimGateEditFilePath(in.ToolInput)
	if !claimGateIsGPUEffectful(fp) {
		return
	}
	path := claimGateStatePath(in.SessionID)
	st, ok := loadEditVerifyGateState(path)
	if !ok {
		st = &editVerifyGateState{SessionID: in.SessionID}
	}
	st.LastEditTS = time.Now().UnixNano()
	st.LastEditFile = fp
	if err := saveEditVerifyGateState(path, st); err != nil {
		hooklog.Writef("claim-gate-track: save error session=%s: %v", in.SessionID, err)
	}
}

func claimGateTrackBash(in event.HookInput) {
	command := claimGateBashCommand(in.ToolInput)
	if command == "" {
		return
	}
	// The handback escape is honored independent of tool_is_error and the
	// verify whitelist — it is a deliberate, explicit operator statement.
	handbackReason := claimGateExtractHandbackReason(command)
	verified := !in.ToolIsError && claimGateCommandIsVerifyWhitelisted(command)
	if handbackReason == "" && !verified {
		return
	}

	path := claimGateStatePath(in.SessionID)
	st, ok := loadEditVerifyGateState(path)
	if !ok {
		st = &editVerifyGateState{SessionID: in.SessionID}
	}
	now := time.Now().UnixNano()
	if verified {
		st.LastVerifyTS = now
		st.LastVerifyCmd = truncate(command, 200)
	}
	if handbackReason != "" {
		st.HandbackReason = truncate(handbackReason, 300)
		st.HandbackTS = now
	}
	if err := saveEditVerifyGateState(path, st); err != nil {
		hooklog.Writef("claim-gate-track: save error session=%s: %v", in.SessionID, err)
	}
}

// cmdClaimGateTrack is the "claim-gate-track" PostToolUse subcommand.
func cmdClaimGateTrack() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return
	}
	switch in.ToolName {
	case "Write", "Edit", "MultiEdit":
		claimGateTrackEdit(in)
	case "Bash":
		claimGateTrackBash(in)
	}
}

// --- decision (Stop) ---

// claimGateMode reads CLAIM_GATE_MODE (default "warn"). Any value other
// than "block" (case-insensitive) is treated as "warn" — fail-safe toward
// the launch default.
func claimGateMode() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CLAIM_GATE_MODE")), "block") {
		return "block"
	}
	return "warn"
}

// claimGateDecision is the pure core of the Stop hook. No ladder: mode is
// a flat config switch, and every Stop while the violation persists
// produces the SAME verdict (warn every time in warn mode; block every
// time in block mode) — per the freeze, escalation-by-repeat-Stop-count
// does not apply to this gate.
func claimGateDecision(st *editVerifyGateState, mode string) (exitCode int, message string) {
	if st.LastEditTS == 0 {
		return 0, "" // no GPU-effectful edit tracked this session
	}
	if st.HandbackTS > 0 && st.HandbackTS >= st.LastEditTS {
		return 0, "" // explicit unverified handback discharges the violation
	}
	if st.LastVerifyTS > 0 && st.LastVerifyTS >= st.LastEditTS {
		return 0, "" // verified after (or at) the edit
	}
	msg := claimGateMessage(st, mode)
	if mode == "block" {
		return 2, msg
	}
	return 0, msg
}

func claimGateFormatTS(ts int64) string {
	if ts == 0 {
		return "never"
	}
	return time.Unix(0, ts).UTC().Format(time.RFC3339)
}

func claimGateMessage(st *editVerifyGateState, mode string) string {
	verb := "WARN"
	if mode == "block" {
		verb = "BLOCK"
	}
	return fmt.Sprintf(
		"CLAIM-GATE %s: %s was edited without a subsequent verify step this session "+
			"(last edit %s, last verify %s). GPU-effectful edits need `shadergen diagnose`, "+
			"`shadergen readiness`, `shadergen screenshot`, a `shadergen step` + `shadergen read` "+
			"pair, `silo_tester --benchmark`, `gpu_verify_lock.sh`, or a pixel-diff/render_metrics "+
			"run before the session ends. Escape (genuinely unverifiable): "+
			"echo UNVERIFIED-HANDBACK:<reason>.",
		verb, st.LastEditFile, claimGateFormatTS(st.LastEditTS), claimGateFormatTS(st.LastVerifyTS))
}

// cmdClaimGate is the "claim-gate" Stop subcommand.
func cmdClaimGate() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return // fail-open: can't resolve a state path
	}
	path := claimGateStatePath(in.SessionID)
	st, ok := loadEditVerifyGateState(path)
	if !ok {
		return // fail-open: no state file, or corrupt JSON
	}

	mode := claimGateMode()
	exitCode, message := claimGateDecision(st, mode)
	if message == "" {
		return
	}
	if exitCode == 2 {
		hooklog.Writef("claim-gate: BLOCK session=%s file=%s", in.SessionID, st.LastEditFile)
		fmt.Fprintln(os.Stderr, message)
		os.Exit(2)
		return
	}
	hooklog.Writef("claim-gate: WARN session=%s file=%s", in.SessionID, st.LastEditFile)
	out := map[string]any{"systemMessage": message}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Println(string(b))
}
