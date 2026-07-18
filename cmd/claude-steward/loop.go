// loop.go — the tick loop and action executor for claude-steward.
//
// # Purpose
//
// This is the daemon's I/O orchestration seam: it drives one tick (Read →
// compute burn → Decide → execute) and maps each pure steward.Action onto the
// concrete edge interface that performs its effect. It depends ONLY on the
// steward interfaces, never on the concrete readers/actuators, so it is fully
// testable with fakes and unaffected by how the edges are implemented.
//
// # Design constraints
//
//   - The executor never panics on a nil edge: a partially-wired daemon (e.g. an
//     observer with no actuator) simply skips the effects it cannot perform.
//   - A failed compaction is logged and the session is left untouched (no retry
//     loop, matching the send-keys safety protocol); it does NOT abort the tick,
//     so budget.json is always refreshed.
//   - Pause is re-checked every tick so `touch ~/.claude/steward/pause` takes
//     effect on the next tick without a restart.
package main

import (
	"context"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

// executor wires the pure core to the concrete edges. Any edge may be nil.
type executor struct {
	reader   steward.Reader
	actuator steward.Actuator
	notifier steward.Notifier
	budget   steward.BudgetWriter
	gate     steward.GateWriter

	state *State
	cfg   steward.Config

	// clock and paused are injectable for tests; defaults set in newExecutor.
	clock  func() time.Time
	paused func() bool
	logf   func(format string, args ...any)
}

// tick performs one full observe-decide-execute cycle. It returns an error only
// on a fatal read failure that yields no usable snapshot; per-action failures
// are logged and swallowed so the loop keeps running.
func (e *executor) tick(ctx context.Context) {
	now := e.clock()

	cfg := e.cfg
	if e.paused != nil {
		cfg.Paused = e.paused()
	}

	snap := steward.Snapshot{Now: now, PrevLevel: e.state.PrevLevel, Config: cfg}
	if e.reader != nil {
		s, err := e.reader.Read(ctx, now, e.state.PrevLevel, cfg)
		if err != nil {
			e.logf("read (partial): %v", err)
		}
		// Use whatever the reader produced, even on a partial error — a missing
		// usage file still lets us observe sessions and write budget.json.
		s.Now, s.PrevLevel, s.Config = now, e.state.PrevLevel, cfg
		snap = s
	}

	// Fold the utilization sample into the burn EMA and overlay per-session
	// compaction history from persisted state.
	snap.BurnPctPerMin = e.state.UpdateBurn(snap.FiveHour.Utilization, now, snap.FiveHour.Present)
	for i := range snap.Sessions {
		snap.Sessions[i].LastCompact = e.state.CompactAgeFor(snap.Sessions[i].ID)
	}

	actions := steward.Decide(snap)
	e.execute(ctx, snap, actions)

	e.state.PrevLevel = levelOf(actions)
	if err := e.state.Save(); err != nil {
		e.logf("state save: %v", err)
	}
}

// execute dispatches each action to its edge. Level and gate are pre-scanned
// because the budget document needs both.
func (e *executor) execute(ctx context.Context, snap steward.Snapshot, actions []steward.Action) {
	level := levelOf(actions)
	gate := gateOf(actions)
	nowRFC := snap.Now.UTC().Format(time.RFC3339)

	for _, a := range actions {
		switch a.Kind {
		case steward.ActionSetBudget:
			if e.budget != nil {
				doc := steward.BuildBudgetDoc(snap, level, gate, nowRFC)
				if err := e.budget.Write(ctx, doc); err != nil {
					e.logf("budget write: %v", err)
				}
			}
		case steward.ActionSetFanoutGate:
			if e.gate != nil {
				if err := e.gate.SetGate(ctx, a.Gate, a.Level); err != nil {
					e.logf("gate write: %v", err)
				}
			}
		case steward.ActionNotify, steward.ActionReportSubagent:
			if e.notifier != nil {
				if err := e.notifier.Notify(ctx, a.Level, a.SessionID, a.Reason); err != nil {
					e.logf("notify: %v", err)
				}
			}
		case steward.ActionCompact:
			if e.actuator == nil {
				continue
			}
			sess, ok := sessionByID(snap.Sessions, a.SessionID)
			if !ok {
				continue
			}
			if err := e.actuator.Compact(ctx, sess, a.Method, a.Reason); err != nil {
				// Leave the session untouched; no retry. Surfaced for the operator.
				e.logf("compact %s via %s: %v", a.SessionID, a.Method, err)
				continue
			}
			e.state.RecordCompact(a.SessionID, snap.Now)
			e.logf("compacted %s via %s: %s", a.SessionID, a.Method, a.Reason)
		}
	}
}

// run drives ticks on the interval until ctx is cancelled. interval <= 0 runs a
// single tick and returns (the --once mode).
func (e *executor) run(ctx context.Context, interval time.Duration) {
	e.tick(ctx)
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.tick(ctx)
		}
	}
}

// levelOf extracts the tick's level from the always-present budget action.
func levelOf(actions []steward.Action) steward.Level {
	for _, a := range actions {
		if a.Kind == steward.ActionSetBudget {
			return a.Level
		}
	}
	return steward.LevelNormal
}

// gateOf extracts the fan-out gate from the actions, defaulting to allow.
func gateOf(actions []steward.Action) steward.FanoutGate {
	for _, a := range actions {
		if a.Kind == steward.ActionSetFanoutGate {
			return a.Gate
		}
	}
	return steward.FanoutAllow
}

func sessionByID(sessions []steward.SessionState, id string) (steward.SessionState, bool) {
	for _, s := range sessions {
		if s.ID == id {
			return s, true
		}
	}
	return steward.SessionState{}, false
}
