package main

import (
	"context"
	"testing"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

// fake edges ---------------------------------------------------------------

type fakeReader struct {
	snaps []steward.Snapshot
	i     int
}

func (f *fakeReader) Read(_ context.Context, _ time.Time, _ steward.Level, _ steward.Config) (steward.Snapshot, error) {
	s := f.snaps[min(f.i, len(f.snaps)-1)]
	f.i++
	return s, nil
}

type fakeActuator struct {
	calls []string
	err   error
}

func (f *fakeActuator) Compact(_ context.Context, s steward.SessionState, m steward.Method, _ string) error {
	f.calls = append(f.calls, s.ID+"/"+m.String())
	return f.err
}

type fakeNotifier struct{ msgs []string }

func (f *fakeNotifier) Notify(_ context.Context, l steward.Level, id, msg string) error {
	f.msgs = append(f.msgs, l.String()+":"+id+":"+msg)
	return nil
}

type fakeBudget struct{ docs []steward.BudgetDoc }

func (f *fakeBudget) Write(_ context.Context, d steward.BudgetDoc) error {
	f.docs = append(f.docs, d)
	return nil
}

type fakeGate struct{ last steward.FanoutGate }

func (f *fakeGate) SetGate(_ context.Context, g steward.FanoutGate, _ steward.Level) error {
	f.last = g
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// helpers ------------------------------------------------------------------

func newTestExec(t *testing.T, r steward.Reader, cfg steward.Config) (*executor, *fakeActuator, *fakeNotifier, *fakeBudget, *fakeGate) {
	t.Helper()
	act := &fakeActuator{}
	note := &fakeNotifier{}
	bud := &fakeBudget{}
	gate := &fakeGate{}
	e := &executor{
		reader: r, actuator: act, notifier: note, budget: bud, gate: gate,
		state:  LoadState(t.TempDir() + "/state.json"),
		cfg:    cfg,
		clock:  func() time.Time { return time.Unix(1_700_000_000, 0) },
		paused: func() bool { return false },
		logf:   func(string, ...any) {},
	}
	return e, act, note, bud, gate
}

func idleSess(id string, ctx int) steward.SessionState {
	return steward.SessionState{
		ID: id, Kind: steward.KindInteractive, Status: "idle",
		ContextPct: ctx, IdleFor: 10 * time.Minute, CacheAge: 40 * time.Minute,
		PaneID: "tmux:%1",
	}
}

// tests --------------------------------------------------------------------

func TestTickWritesBudgetAndNotifiesOnRise(t *testing.T) {
	cfg := steward.DefaultConfig() // observer: notify on, compact off
	now := time.Unix(1_700_000_000, 0)
	snap := steward.Snapshot{FiveHour: steward.Window{Utilization: 65, ResetsAt: now.Add(3 * time.Hour), Present: true}}
	e, _, note, bud, _ := newTestExec(t, &fakeReader{snaps: []steward.Snapshot{snap}}, cfg)

	e.tick(context.Background())

	if len(bud.docs) != 1 || bud.docs[0].State != "watch" {
		t.Fatalf("expected one budget doc at watch, got %+v", bud.docs)
	}
	if len(note.msgs) != 1 {
		t.Fatalf("expected one rise notification, got %v", note.msgs)
	}
	if e.state.PrevLevel != steward.LevelWatch {
		t.Fatalf("prev level should persist as watch, got %v", e.state.PrevLevel)
	}
}

func TestBurnRateFeedsForecastEscalation(t *testing.T) {
	cfg := steward.DefaultConfig()
	base := time.Unix(1_700_000_000, 0)
	reset := base.Add(3 * time.Hour)
	// Two ticks two minutes apart, utilization 40 → 44: a real positive burn
	// with the window far from reset must escalate to PRESSURE by forecast even
	// though 44% is well under the 75% pressure threshold.
	s1 := steward.Snapshot{FiveHour: steward.Window{Utilization: 40, ResetsAt: reset, Present: true}}
	s2 := steward.Snapshot{FiveHour: steward.Window{Utilization: 44, ResetsAt: reset, Present: true}}
	e, _, _, bud, _ := newTestExec(t, &fakeReader{snaps: []steward.Snapshot{s1, s2}}, cfg)

	times := []time.Time{base, base.Add(2 * time.Minute)}
	i := 0
	e.clock = func() time.Time { t := times[min(i, len(times)-1)]; i++; return t }

	e.tick(context.Background()) // primes burn baseline
	e.tick(context.Background()) // computes burn, should escalate
	if got := bud.docs[len(bud.docs)-1].State; got != "pressure" {
		t.Fatalf("second tick should escalate to pressure by forecast, got %q (burn=%.2f)", got, e.state.BurnEMA)
	}
}

func TestCompactRecordsHistoryAndRespectsError(t *testing.T) {
	cfg := steward.DefaultConfig()
	cfg.EnableCompact = true
	now := time.Unix(1_700_000_000, 0)
	snap := steward.Snapshot{
		FiveHour: steward.Window{Utilization: 92, ResetsAt: now.Add(3 * time.Hour), Present: true},
		Sessions: []steward.SessionState{idleSess("s1", 70)},
	}
	e, act, _, _, _ := newTestExec(t, &fakeReader{snaps: []steward.Snapshot{snap}}, cfg)
	e.tick(context.Background())
	if len(act.calls) != 1 || act.calls[0] != "s1/tmux" {
		t.Fatalf("expected one tmux compact of s1, got %v", act.calls)
	}
	if e.state.CompactAgeFor("s1").IsZero() {
		t.Fatal("successful compact should record last-compact time")
	}

	// Now a failing actuator must NOT record history (so cooldown won't wrongly suppress).
	e2, act2, _, _, _ := newTestExec(t, &fakeReader{snaps: []steward.Snapshot{snap}}, cfg)
	act2.err = context.DeadlineExceeded
	e2.tick(context.Background())
	if !e2.state.CompactAgeFor("s1").IsZero() {
		t.Fatal("failed compact must not record last-compact time")
	}
}

func TestPauseSuppressesCompaction(t *testing.T) {
	cfg := steward.DefaultConfig()
	cfg.EnableCompact = true
	now := time.Unix(1_700_000_000, 0)
	snap := steward.Snapshot{
		FiveHour: steward.Window{Utilization: 95, ResetsAt: now.Add(3 * time.Hour), Present: true},
		Sessions: []steward.SessionState{idleSess("s1", 70)},
	}
	e, act, _, _, _ := newTestExec(t, &fakeReader{snaps: []steward.Snapshot{snap}}, cfg)
	e.paused = func() bool { return true }
	e.tick(context.Background())
	if len(act.calls) != 0 {
		t.Fatalf("paused steward must not compact, got %v", act.calls)
	}
}
