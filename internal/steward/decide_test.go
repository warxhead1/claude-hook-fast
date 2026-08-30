package steward

import (
	"testing"
	"time"
)

// base returns a Snapshot with observer+compact+gate+block all enabled so a
// test can exercise any phase; individual tests flip gates off as needed.
func base(util float64, prev Level) Snapshot {
	cfg := DefaultConfig()
	cfg.EnableRulesGate = true
	cfg.EnableCompact = true
	cfg.EnableFanoutBlock = true
	now := time.Unix(1_700_000_000, 0)
	return Snapshot{
		Now:       now,
		FiveHour:  Window{Utilization: util, ResetsAt: now.Add(3 * time.Hour), Present: true},
		PrevLevel: prev,
		Config:    cfg,
	}
}

func has(actions []Action, k ActionKind) bool {
	for _, a := range actions {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func compactIDs(actions []Action) []string {
	var ids []string
	for _, a := range actions {
		if a.Kind == ActionCompact {
			ids = append(ids, a.SessionID)
		}
	}
	return ids
}

func TestComputeLevel(t *testing.T) {
	tests := []struct {
		name string
		util float64
		prev Level
		want Level
	}{
		{"normal", 10, LevelNormal, LevelNormal},
		{"watch entry", 60, LevelNormal, LevelWatch},
		{"pressure entry", 75, LevelWatch, LevelPressure},
		{"critical entry", 90, LevelPressure, LevelCritical},
		{"just below watch", 59, LevelNormal, LevelNormal},
		// Hysteresis: falling from critical, still within 5 of 90 → stays.
		{"hysteresis holds critical", 86, LevelCritical, LevelCritical},
		{"hysteresis releases critical", 84, LevelCritical, LevelPressure},
		// A large genuine drop steps down past hysteresis.
		{"big drop from critical", 62, LevelCritical, LevelWatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeLevel(base(tc.util, tc.prev))
			if got != tc.want {
				t.Fatalf("util=%.0f prev=%v: got %v want %v", tc.util, tc.prev, got, tc.want)
			}
		})
	}
}

func TestForecastEscalation(t *testing.T) {
	snap := base(40, LevelNormal) // only WATCH by percentage
	// Burn 2%/min with 40% left → exhausts in ~20min, but window resets in 3h.
	snap.BurnPctPerMin = 2
	if got := computeLevel(snap); got != LevelPressure {
		t.Fatalf("forecast should escalate WATCH→PRESSURE, got %v", got)
	}
	// Same burn but the window resets in 5 min → no escalation (reset saves us).
	snap.FiveHour.ResetsAt = snap.Now.Add(5 * time.Minute)
	if got := computeLevel(snap); got == LevelPressure {
		t.Fatalf("no escalation when reset precedes exhaustion, got %v", got)
	}
}

func TestBudgetAlwaysEmitted(t *testing.T) {
	snap := base(5, LevelNormal)
	snap.Config = DefaultConfig() // pure observer, everything else off
	got := Decide(snap)
	if len(got) != 1 || got[0].Kind != ActionSetBudget {
		t.Fatalf("observer tick should emit exactly ActionSetBudget, got %+v", got)
	}
}

func TestNotifyOnlyOnRise(t *testing.T) {
	// Rising into WATCH notifies.
	if !has(Decide(base(65, LevelNormal)), ActionNotify) {
		t.Fatal("expected notify on rise into WATCH")
	}
	// Staying at WATCH does not re-notify.
	if has(Decide(base(65, LevelWatch)), ActionNotify) {
		t.Fatal("should not re-notify at steady level")
	}
}

func TestFanoutGate(t *testing.T) {
	tests := []struct {
		util  float64
		block bool
		want  FanoutGate
	}{
		{50, true, FanoutAllow},
		{78, true, FanoutWarn},
		{95, false, FanoutWarn}, // critical but Phase-3 block disabled
		{95, true, FanoutBlock}, // critical with block enabled
	}
	for _, tc := range tests {
		snap := base(tc.util, LevelNormal)
		snap.Config.EnableFanoutBlock = tc.block
		got := fanoutGate(computeLevel(snap), snap.Config)
		if got != tc.want {
			t.Fatalf("util=%.0f block=%v: got %v want %v", tc.util, tc.block, got, tc.want)
		}
	}
}

func idleSession(id string, ctx int) SessionState {
	return SessionState{
		ID:         id,
		Kind:       KindInteractive,
		Status:     "idle",
		ContextPct: ctx,
		IdleFor:    10 * time.Minute,
		CacheAge:   30 * time.Minute,
		PaneID:     "tmux:%1",
	}
}

func TestCompactEligibility(t *testing.T) {
	snap := base(80, LevelPressure)
	ok := idleSession("ok", 70)

	busy := idleSession("busy", 70)
	busy.Status = "busy"
	waiting := idleSession("waiting", 70)
	waiting.Status = "waiting"
	lowctx := idleSession("lowctx", 40)
	fresh := idleSession("fresh", 70)
	fresh.IdleFor = 30 * time.Second
	withsub := idleSession("withsub", 70)
	withsub.SubagentsActive = 1
	ignored := idleSession("ignored", 70)
	ignored.Ignored = true
	unreachable := idleSession("unreachable", 70)
	unreachable.PaneID = ""
	cooling := idleSession("cooling", 70)
	cooling.LastCompact = snap.Now.Add(-5 * time.Minute)

	snap.Sessions = []SessionState{ok, busy, waiting, lowctx, fresh, withsub, ignored, unreachable, cooling}
	ids := compactIDs(Decide(snap))
	if len(ids) != 1 || ids[0] != "ok" {
		t.Fatalf("only the clean idle session should be eligible, got %v", ids)
	}
}

func TestCompactRankingAndCap(t *testing.T) {
	snap := base(92, LevelCritical)
	snap.Config.MaxCompactsPerTick = 2
	// s_old has the oldest cache (closest to expiry) → highest priority.
	sOld := idleSession("old", 60)
	sOld.CacheAge = 50 * time.Minute
	sBig := idleSession("big", 90)
	sBig.CacheAge = 20 * time.Minute
	sSmall := idleSession("small", 60)
	sSmall.CacheAge = 20 * time.Minute
	snap.Sessions = []SessionState{sSmall, sBig, sOld}
	ids := compactIDs(Decide(snap))
	if len(ids) != 2 {
		t.Fatalf("cap should limit to 2 compacts, got %v", ids)
	}
	if ids[0] != "old" || ids[1] != "big" {
		t.Fatalf("ranking should be cache-expiry then context: got %v", ids)
	}
}

func TestNoCompactNearReset(t *testing.T) {
	snap := base(92, LevelCritical)
	snap.FiveHour.ResetsAt = snap.Now.Add(10 * time.Minute) // < MinResetHeadroom (20m)
	snap.Sessions = []SessionState{idleSession("ok", 70)}
	if has(Decide(snap), ActionCompact) {
		t.Fatal("should not compact when the window resets within headroom")
	}
}

func TestPauseHaltsActuation(t *testing.T) {
	snap := base(95, LevelCritical)
	snap.Config.Paused = true
	snap.Sessions = []SessionState{idleSession("ok", 70)}
	got := Decide(snap)
	if has(got, ActionCompact) {
		t.Fatal("paused steward must not compact")
	}
	// Gate must not escalate to block while paused.
	for _, a := range got {
		if a.Kind == ActionSetFanoutGate && a.Gate == FanoutBlock {
			t.Fatal("paused steward must not block fan-out")
		}
	}
	// But it still observes.
	if !has(got, ActionSetBudget) {
		t.Fatal("paused steward still writes budget")
	}
}

func TestCompactGateOff(t *testing.T) {
	snap := base(95, LevelCritical)
	snap.Config.EnableCompact = false // Phase 0/1
	snap.Sessions = []SessionState{idleSession("ok", 70)}
	if has(Decide(snap), ActionCompact) {
		t.Fatal("compaction must not fire when EnableCompact is off")
	}
}

func TestLingeringSubagent(t *testing.T) {
	snap := base(30, LevelNormal)
	snap.Subagents = []SubagentState{
		{ID: "a", ParentID: "p1", Name: "stuck", Started: true, Stopped: false, StaleFor: 15 * time.Minute},
		{ID: "b", ParentID: "p2", Name: "fine", Started: true, Stopped: false, StaleFor: 2 * time.Minute},
		{ID: "c", ParentID: "p3", Name: "done", Started: true, Stopped: true, StaleFor: 30 * time.Minute},
	}
	got := Decide(snap)
	var reported []string
	for _, a := range got {
		if a.Kind == ActionReportSubagent {
			reported = append(reported, a.SessionID)
		}
	}
	if len(reported) != 1 || reported[0] != "p1" {
		t.Fatalf("only the stalled live subagent's parent should be reported, got %v", reported)
	}
}

func TestResolveActuation(t *testing.T) {
	tests := []struct {
		name string
		s    SessionState
		want Method
	}{
		{"paned", SessionState{PaneID: "tmux:%1"}, MethodTmux},
		{"bg worker", SessionState{PtySock: "/s.sock", PtyAuthAvail: true}, MethodPtySock},
		{"bg no auth", SessionState{PtySock: "/s.sock", PtyAuthAvail: false}, MethodNone},
		{"pane wins over sock", SessionState{PaneID: "tmux:%1", PtySock: "/s.sock", PtyAuthAvail: true}, MethodTmux},
		{"unreachable", SessionState{}, MethodNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveActuation(tc.s); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
