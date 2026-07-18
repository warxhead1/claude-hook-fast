package main

// cmd_bead_claim_gate.go — Bead-claim-gate: catches agents that `bd update
// <id> --claim` a bead and then end the session without any evidenced
// discharge (close / comment / verify_pending disposition / explicit
// unverified handback). Prose rules ("close or evidence your claims")
// haven't stopped this — it becomes a hook, anchored on INVOCATIONS per
// feedback_hooks_match_invocations_not_prose, never on transcript prose
// scanning.
//
// This is a repo-local bonus companion to the frozen T6 claim-gate
// contract in cmd_claim_gate.go (edit-without-verify gate) — undischarged
// bd claims are a related but distinct root cause of the same
// zombie-bead-work problem. Deliberately NOT part of the T6 freeze; the
// canonical `claim-gate`/`claim-gate-track` subcommand names went to that
// contract, so this one is prefixed `bead-claim-gate`.
//
// Two subcommands:
//
//   - bead-claim-gate-track (PostToolUse, Bash) — accumulate-don't-scan.
//     Parses the successful Bash invocation for a `bd update <id> --claim`,
//     and separately for discharge invocations (`bd close <id>`, `bd
//     comment <id> ...`, `verify_pending.py close|parked <id>`, or the
//     `UNVERIFIED-HANDBACK:<id>` escape token), and folds the result into
//     /tmp/claude-bead-claim-gate-<sessionID>.json. Never reads the transcript.
//
//   - bead-claim-gate (Stop) — O(1) read of that same state file. No
//     undischarged claims -> exit 0 silently. Undischarged claims present
//     for the FIRST time (or with a changed set) -> exit 0 + systemMessage
//     warning (ladder rung 1), and the state file records which bead IDs
//     were warned about. A SECOND Stop with the SAME undischarged set ->
//     exit 2 + stderr listing the beads and discharge options (ladder
//     rung 2, blocks the stop).
//
// Fail-open everywhere: missing state file, corrupt JSON, missing
// session_id, non-Bash tool, non-claiming session -> exit 0. A broken gate
// must never brick a session.
//
// Exit 0 = allow stop / tracked silently; exit 2 = block stop
// (bead-claim-gate only; bead-claim-gate-track never blocks, it is
// PostToolUse telemetry).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

// --- discharge/claim event kinds ---

const (
	beadClaimEventClaim        = "claim"
	beadClaimEventBdClose      = "bd_close"
	beadClaimEventBdComment    = "bd_comment"
	beadClaimEventVerifyClose  = "verify_close"
	beadClaimEventVerifyParked = "verify_parked"
	beadClaimEventHandback     = "handback"
)

// beadClaimDischargeKinds are the event kinds that discharge a tracked claim.
var beadClaimDischargeKinds = map[string]bool{
	beadClaimEventBdClose:      true,
	beadClaimEventBdComment:    true,
	beadClaimEventVerifyClose:  true,
	beadClaimEventVerifyParked: true,
	beadClaimEventHandback:     true,
}

type beadClaimEvent struct {
	Kind   string
	BeadID string
}

// --- state file shape ---

type beadClaimRecord struct {
	BeadID          string `json:"bead_id"`
	ClaimedAt       string `json:"claimed_at"`
	Discharged      bool   `json:"discharged"`
	DischargedAt    string `json:"discharged_at,omitempty"`
	DischargeReason string `json:"discharge_reason,omitempty"`
}

type beadClaimGateState struct {
	SessionID string                      `json:"session_id"`
	Claims    map[string]*beadClaimRecord `json:"claims"`
	Warned    bool                        `json:"warned"`
	WarnedIDs []string                    `json:"warned_ids,omitempty"`
}

// beadClaimGateStateDir is the directory holding per-session state files.
// A package var (not an env var) so tests can redirect it, matching the
// prSummaryPath/prLogPath convention in cmd_plan_resume.go.
var beadClaimGateStateDir = "/tmp"

func beadClaimGateStatePath(sessionID string) string {
	return filepath.Join(beadClaimGateStateDir, fmt.Sprintf("claude-bead-claim-gate-%s.json", sessionID))
}

var beadClaimHandbackRe = regexp.MustCompile(`UNVERIFIED-HANDBACK:([A-Za-z0-9._-]+)`)

// detectBeadClaimEvents parses a Bash command string (may contain
// &&/||/;/newline-separated segments) for claim and discharge invocations.
// Pure function, fail-open on anything unparseable (skips the segment).
func detectBeadClaimEvents(command string) []beadClaimEvent {
	var events []beadClaimEvent

	// The UNVERIFIED-HANDBACK escape is matched against the raw command
	// text (not per-segment tokens) so quoting around the reason text
	// never interferes with recognizing the marker.
	for _, m := range beadClaimHandbackRe.FindAllStringSubmatch(command, -1) {
		events = append(events, beadClaimEvent{Kind: beadClaimEventHandback, BeadID: m[1]})
	}

	for _, seg := range segSplitRe.Split(command, -1) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		tokens, err := shellSplit(seg)
		if err != nil || len(tokens) == 0 {
			continue
		}
		tokens = beadClaimStripWrappers(tokens)
		if len(tokens) == 0 {
			continue
		}

		base := filepath.Base(tokens[0])
		if base == "bd" {
			if len(tokens) < 3 {
				continue
			}
			sub := tokens[1]
			rest := tokens[2:]
			id := beadClaimFirstPositional(rest)
			if id == "" {
				continue
			}
			switch sub {
			case "update":
				if beadClaimHasFlag(rest, "--claim") {
					events = append(events, beadClaimEvent{Kind: beadClaimEventClaim, BeadID: id})
				}
			case "close":
				events = append(events, beadClaimEvent{Kind: beadClaimEventBdClose, BeadID: id})
			case "comment":
				events = append(events, beadClaimEvent{Kind: beadClaimEventBdComment, BeadID: id})
			}
			continue
		}

		if idx := beadClaimVerifyPendingIndex(tokens); idx >= 0 {
			sub, id := beadClaimFirstTwoPositionals(tokens[idx+1:])
			if id == "" {
				continue
			}
			switch sub {
			case "close":
				events = append(events, beadClaimEvent{Kind: beadClaimEventVerifyClose, BeadID: id})
			case "parked":
				events = append(events, beadClaimEvent{Kind: beadClaimEventVerifyParked, BeadID: id})
			}
		}
	}
	return events
}

// beadClaimStripWrappers drops leading shell keywords/wrappers and
// NAME=value assignments so the real command word surfaces. Reuses
// stripTokens/assignRe from cmd_worktree_bash_guard.go.
func beadClaimStripWrappers(tokens []string) []string {
	for len(tokens) > 0 {
		t0 := tokens[0]
		if stripTokens[t0] {
			tokens = tokens[1:]
			continue
		}
		if assignRe.MatchString(t0) {
			tokens = tokens[1:]
			continue
		}
		break
	}
	return tokens
}

func beadClaimFirstPositional(tokens []string) string {
	for _, t := range tokens {
		if t == "" || strings.HasPrefix(t, "-") {
			continue
		}
		return t
	}
	return ""
}

// beadClaimFirstTwoPositionals returns the first two non-flag tokens (subcommand,
// id). Returns ("", "") when fewer than two are found.
func beadClaimFirstTwoPositionals(tokens []string) (string, string) {
	var found []string
	for _, t := range tokens {
		if t == "" || strings.HasPrefix(t, "-") {
			continue
		}
		found = append(found, t)
		if len(found) == 2 {
			break
		}
	}
	if len(found) < 2 {
		return "", ""
	}
	return found[0], found[1]
}

func beadClaimHasFlag(tokens []string, flag string) bool {
	for _, t := range tokens {
		if t == flag {
			return true
		}
	}
	return false
}

// beadClaimVerifyPendingIndex returns the index of a verify_pending.py invocation
// token within tokens (handles `python3 .../verify_pending.py ...` as well
// as a bare `verify_pending.py ...`), or -1 if not present.
func beadClaimVerifyPendingIndex(tokens []string) int {
	for i, t := range tokens {
		b := filepath.Base(t)
		if b == "verify_pending.py" || b == "verify_pending" {
			return i
		}
	}
	return -1
}

// --- state I/O ---

// loadBeadClaimGateState reads and parses the state file. ok=false on any
// error (missing file, corrupt JSON) — callers must fail open.
func loadBeadClaimGateState(path string) (*beadClaimGateState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var st beadClaimGateState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false
	}
	if st.Claims == nil {
		st.Claims = map[string]*beadClaimRecord{}
	}
	return &st, true
}

func saveBeadClaimGateState(path string, st *beadClaimGateState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// applyBeadClaimEvents folds parsed events into state. toolIsError gates
// EVERY event kind (claim and discharge alike): a failed invocation proves
// nothing, so it is not recorded. This is the "confirm success from the
// tool result if available, else record optimistically" rule — ToolIsError
// is only populated by Claude Code ~2.1+; when it's unset (false, the zero
// value) events are recorded optimistically.
func applyBeadClaimEvents(st *beadClaimGateState, events []beadClaimEvent, now time.Time, toolIsError bool) {
	if toolIsError {
		return
	}
	if st.Claims == nil {
		st.Claims = map[string]*beadClaimRecord{}
	}
	ts := now.UTC().Format(time.RFC3339Nano)
	for _, ev := range events {
		if ev.Kind == beadClaimEventClaim {
			c, exists := st.Claims[ev.BeadID]
			if !exists {
				c = &beadClaimRecord{BeadID: ev.BeadID}
				st.Claims[ev.BeadID] = c
			}
			c.ClaimedAt = ts
			c.Discharged = false
			c.DischargedAt = ""
			c.DischargeReason = ""
			continue
		}
		if beadClaimDischargeKinds[ev.Kind] {
			c, exists := st.Claims[ev.BeadID]
			if !exists {
				// Discharge references a bead this session never tracked a
				// claim for (claimed in a prior session, or never claimed
				// at all) — nothing to discharge, no-op.
				continue
			}
			c.Discharged = true
			c.DischargedAt = ts
			c.DischargeReason = ev.Kind
		}
	}
}

// beadClaimUndischargedIDs returns the sorted bead IDs with an open (undischarged)
// claim in this session's state.
func beadClaimUndischargedIDs(st *beadClaimGateState) []string {
	var ids []string
	for id, c := range st.Claims {
		if c != nil && !c.Discharged {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func beadClaimSameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// beadClaimGateDecision is the pure core of the Stop-hook ladder. Mutates st
// in place (Warned/WarnedIDs) when it decides to warn, so the caller can
// persist the updated state.
//
//	no undischarged claims        -> exit 0, no message
//	undischarged set changed/new  -> exit 0, warn message (rung 1)
//	same undischarged set again   -> exit 2, block message (rung 2)
func beadClaimGateDecision(st *beadClaimGateState) (exitCode int, message string) {
	undis := beadClaimUndischargedIDs(st)
	if len(undis) == 0 {
		return 0, ""
	}
	if st.Warned && beadClaimSameIDs(st.WarnedIDs, undis) {
		return 2, beadClaimBlockMessage(undis)
	}
	st.Warned = true
	st.WarnedIDs = undis
	return 0, beadClaimWarnMessage(undis)
}

func beadClaimWarnMessage(ids []string) string {
	return fmt.Sprintf(
		"BEAD-CLAIM-GATE WARN: %d bead(s) claimed this session have no evidenced discharge: %s\n"+
			"Discharge with `bd close <id>`, `bd comment <id> \"<note>\"`, "+
			"`verify_pending.py close|parked <id>`, or (if genuinely unverifiable) "+
			"`echo UNVERIFIED-HANDBACK:<id> \"<reason>\"`. Stopping again without "+
			"discharging will BLOCK.", len(ids), strings.Join(ids, ", "))
}

func beadClaimBlockMessage(ids []string) string {
	return fmt.Sprintf(
		"BEAD-CLAIM-GATE BLOCK: %d bead(s) claimed this session remain undischarged: %s\n\n"+
			"Discharge options:\n"+
			"  bd close <id>                              — work complete\n"+
			"  bd comment <id> \"<note>\"                    — evidence progress, keep claim open\n"+
			"  verify_pending.py close <id>                — verify-pipeline close\n"+
			"  verify_pending.py parked <id>                — verify-pipeline park\n"+
			"  echo UNVERIFIED-HANDBACK:<id> \"<reason>\"     — explicit unverified handback\n",
		len(ids), strings.Join(ids, ", "))
}

// --- subcommand entry points ---

// cmdBeadClaimGateTrack is the "bead-claim-gate-track" PostToolUse subcommand.
func cmdBeadClaimGateTrack() {
	in, _ := readStdinForHook()
	if in.SessionID == "" || in.ToolName != "Bash" {
		return
	}
	var ti struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(in.ToolInput, &ti); err != nil || ti.Command == "" {
		return
	}
	events := detectBeadClaimEvents(ti.Command)
	if len(events) == 0 {
		return
	}

	path := beadClaimGateStatePath(in.SessionID)
	st, ok := loadBeadClaimGateState(path)
	if !ok {
		st = &beadClaimGateState{SessionID: in.SessionID, Claims: map[string]*beadClaimRecord{}}
	}
	applyBeadClaimEvents(st, events, time.Now(), in.ToolIsError)
	if err := saveBeadClaimGateState(path, st); err != nil {
		hooklog.Writef("bead-claim-gate-track: save error session=%s: %v", in.SessionID, err)
	}
}

// cmdBeadClaimGate is the "bead-claim-gate" Stop subcommand.
func cmdBeadClaimGate() {
	in, _ := readStdinForHook()
	if in.SessionID == "" {
		return // fail-open: can't resolve a state path
	}
	path := beadClaimGateStatePath(in.SessionID)
	st, ok := loadBeadClaimGateState(path)
	if !ok {
		return // fail-open: no state file, or corrupt JSON
	}

	exitCode, message := beadClaimGateDecision(st)
	switch exitCode {
	case 0:
		if message == "" {
			return
		}
		if err := saveBeadClaimGateState(path, st); err != nil {
			hooklog.Writef("bead-claim-gate: save error session=%s: %v", in.SessionID, err)
		}
		out := map[string]any{"systemMessage": message}
		b, err := json.Marshal(out)
		if err != nil {
			return
		}
		fmt.Println(string(b))
	default: // 2 — block
		hooklog.Writef("bead-claim-gate: BLOCK session=%s beads=%v", in.SessionID, st.WarnedIDs)
		fmt.Fprintln(os.Stderr, message)
		hookExit(2)
	}
}
