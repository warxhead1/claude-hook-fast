// state.go — cross-tick daemon state for claude-steward.
//
// # Purpose
//
// The pure policy core (internal/steward) is stateless: every tick it is handed
// a complete Snapshot. But a few quantities are inherently cross-tick — the
// previous pressure level (for hysteresis and edge-triggered notifications), the
// burn-rate EMA (utilization is only sampled every ~120s, so we smooth the
// first-difference), and each session's last-compaction time (to enforce the
// cooldown across restarts). This file owns that small persisted state.
//
// # Design constraints
//
//   - Restartable: the whole State round-trips through one JSON file so a daemon
//     restart resumes with correct hysteresis and cooldowns.
//   - Atomic persistence: write-temp-then-rename, never a half-written file.
//   - Graceful: a missing or corrupt state file yields a zero State, never a
//     crash — the steward simply starts as if fresh.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

// burnAlpha is the EMA smoothing factor for the utilization burn rate. 0.3
// weights recent samples enough to react to a spike within a few ticks while
// riding out the quantization noise of integer-percent utilization sampled only
// every ~120s.
const burnAlpha = 0.3

// State is the persisted cross-tick daemon state.
type State struct {
	PrevLevel steward.Level `json:"prev_level"`

	// Burn-rate tracking.
	BurnEMA      float64   `json:"burn_ema_pct_per_min"`
	LastUtil     float64   `json:"last_util"`
	LastUtilTime time.Time `json:"last_util_time"`

	// LastCompact maps session ID → last compaction time (unix seconds via JSON
	// time) so the cooldown survives restarts.
	LastCompact map[string]time.Time `json:"last_compact"`

	path string
}

// LoadState reads the state file at path, returning a zero-valued State (with
// the path set) on any missing/corrupt file.
func LoadState(path string) *State {
	s := &State{path: path, LastCompact: map[string]time.Time{}}
	data, err := os.ReadFile(path) //nolint:gosec // operator-owned path
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s) // corrupt file → keep the zero State
	if s.LastCompact == nil {
		s.LastCompact = map[string]time.Time{}
	}
	s.path = path
	return s
}

// Save atomically persists the state. Errors are returned but are non-fatal to
// the caller — a lost state write only costs one tick of hysteresis memory.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// UpdateBurn folds a new utilization sample into the EMA and returns the current
// burn rate in percent-per-minute. A window reset (utilization drops) or an
// absent sample contributes a zero instantaneous rate rather than a negative
// one, so the forecast never reads as "negative burn".
func (s *State) UpdateBurn(util float64, now time.Time, present bool) float64 {
	if !present {
		return s.BurnEMA
	}
	if !s.LastUtilTime.IsZero() && now.After(s.LastUtilTime) {
		mins := now.Sub(s.LastUtilTime).Minutes()
		if mins > 0 {
			inst := (util - s.LastUtil) / mins
			if inst < 0 { // window reset or noise — floor at zero
				inst = 0
			}
			s.BurnEMA = burnAlpha*inst + (1-burnAlpha)*s.BurnEMA
		}
	}
	s.LastUtil = util
	s.LastUtilTime = now
	return s.BurnEMA
}

// RecordCompact stamps a session's last-compaction time.
func (s *State) RecordCompact(sessionID string, now time.Time) {
	if s.LastCompact == nil {
		s.LastCompact = map[string]time.Time{}
	}
	s.LastCompact[sessionID] = now
}

// CompactAgeFor returns the recorded last-compaction time for a session (zero if
// never), used to overlay SessionState.LastCompact before Decide runs.
func (s *State) CompactAgeFor(sessionID string) time.Time {
	return s.LastCompact[sessionID]
}
