package steward

import "sort"

// Decide is the pure policy core. Given a Snapshot of the world it returns the
// list of Actions the daemon should execute this tick. It performs no I/O and
// reads no clock — every input is on the Snapshot, every effect is an Action.
//
// The four rollout phases are expressed entirely through Config gates:
//
//   - Phase 0 (observer): always emits ActionSetBudget; EnableNotify adds
//     level-change notifications. No compaction, no gating.
//   - Phase 1 (advisory): EnableRulesGate publishes the fan-out gate
//     (allow/warn) for the rules engine.
//   - Phase 2 (actuation): EnableCompact emits ActionCompact for eligible idle
//     sessions, ranked by cache-expiry urgency.
//   - Phase 3 (hardening): EnableFanoutBlock escalates the gate to block at
//     CRITICAL.
//
// Paused halts actuation (no Compact; block downgraded to warn) while still
// observing and notifying.
func Decide(snap Snapshot) []Action {
	cfg := snap.Config
	level := computeLevel(snap)

	actions := make([]Action, 0, 4)

	// (1) The public contract is always refreshed, at every level, every tick.
	actions = append(actions, Action{
		Kind:   ActionSetBudget,
		Level:  level,
		Reason: "budget snapshot",
	})

	// (2) Fan-out gate for the rules engine (Phase 1+).
	if cfg.EnableRulesGate {
		actions = append(actions, Action{
			Kind:   ActionSetFanoutGate,
			Level:  level,
			Gate:   fanoutGate(level, cfg),
			Reason: "fan-out gate for level " + level.String(),
		})
	}

	// (3) Notify once on an upward level change (Phase 0/1).
	if cfg.EnableNotify && level >= LevelWatch && level > snap.PrevLevel {
		actions = append(actions, Action{
			Kind:   ActionNotify,
			Level:  level,
			Reason: "budget level rose to " + level.String(),
		})
	}

	// (4) Lingering-subagent reports (independent of level — a stalled subagent
	// is worth surfacing whenever it is detected, and its parent must be
	// excluded from compaction below).
	if cfg.EnableNotify {
		for _, sub := range snap.Subagents {
			if sub.Lingering(cfg.LingeringSubagentAfter) {
				actions = append(actions, Action{
					Kind:      ActionReportSubagent,
					Level:     level,
					SessionID: sub.ParentID,
					Reason:    "lingering subagent " + sub.Name,
				})
			}
		}
	}

	// (5) Compaction (Phase 2), only under real pressure and never while paused.
	if cfg.EnableCompact && !cfg.Paused && level >= LevelPressure {
		// If the window resets very soon, do nothing — the reset solves the
		// pressure for free and compacting would waste the sessions' warm state.
		if ttr := snap.TimeToReset(); ttr == 0 || ttr >= cfg.MinResetHeadroom {
			for _, s := range rankEligible(snap) {
				actions = append(actions, Action{
					Kind:      ActionCompact,
					Level:     level,
					SessionID: s.ID,
					Method:    ResolveActuation(s),
					Reason:    compactReason(s, level),
				})
			}
		}
	}

	return actions
}

// computeLevel maps five-hour utilization to a Level, escalates on an
// exhaust-before-reset forecast, and applies downward hysteresis so the
// steward does not flap at a threshold boundary.
func computeLevel(snap Snapshot) Level {
	cfg := snap.Config
	u := snap.FiveHour.Utilization

	level := LevelNormal
	if u >= cfg.WatchPct {
		level = LevelWatch
	}
	if u >= cfg.PressurePct {
		level = LevelPressure
	}
	if u >= cfg.CriticalPct {
		level = LevelCritical
	}

	// Forecast escalation: if we are on track to exhaust the window before it
	// resets, treat it as at least PRESSURE regardless of the instantaneous
	// percentage.
	if fe := snap.ForecastExhaust(); fe > 0 {
		if ttr := snap.TimeToReset(); ttr > 0 && fe < ttr && level < LevelPressure {
			level = LevelPressure
		}
	}

	// Hysteresis: resist stepping DOWN. Only drop below the previous level once
	// utilization has fallen clearly (by Hysteresis points) below that level's
	// entry threshold. Upward transitions are never damped.
	if level < snap.PrevLevel {
		if u > entryThreshold(snap.PrevLevel, cfg)-cfg.Hysteresis {
			level = snap.PrevLevel
		}
	}

	return level
}

// entryThreshold is the utilization at which a level is entered.
func entryThreshold(l Level, cfg Config) float64 {
	switch l {
	case LevelWatch:
		return cfg.WatchPct
	case LevelPressure:
		return cfg.PressurePct
	case LevelCritical:
		return cfg.CriticalPct
	default:
		return 0
	}
}

// fanoutGate maps a level to the rules-engine gate value, honoring the
// EnableFanoutBlock (Phase 3) gate and the pause switch.
func fanoutGate(level Level, cfg Config) FanoutGate {
	switch level {
	case LevelPressure:
		return FanoutWarn
	case LevelCritical:
		if cfg.EnableFanoutBlock && !cfg.Paused {
			return FanoutBlock
		}
		return FanoutWarn
	default:
		return FanoutAllow
	}
}

// compactEligible reports whether a session may be compacted right now. It is
// deliberately strict: only a genuinely idle, context-heavy, reachable session
// with no live subagents and past its cooldown qualifies.
func compactEligible(s SessionState, snap Snapshot) bool {
	cfg := snap.Config
	switch {
	case s.Ignored:
		return false
	case s.Status != "idle": // never "busy" (mid-turn) or "waiting" (prompt up)
		return false
	case s.IdleFor < cfg.MinIdleForCompact:
		return false
	case s.ContextPct < cfg.MinContextPct:
		return false
	case s.SubagentsActive > 0:
		return false
	case !s.LastCompact.IsZero() && snap.Now.Sub(s.LastCompact) < cfg.CompactCooldown:
		return false
	case ResolveActuation(s) == MethodNone:
		return false
	default:
		return true
	}
}

// rankEligible returns the compact-eligible sessions in priority order and caps
// the slice at MaxCompactsPerTick. Ranking favors, in order: sessions whose
// prompt cache is closest to expiry (largest CacheAge — compacting before the
// 1-hour TTL lapses reads mostly-cached input and leaves a cheap-to-reprime
// context), then the largest context, then the longest idle.
func rankEligible(snap Snapshot) []SessionState {
	var elig []SessionState
	for _, s := range snap.Sessions {
		if compactEligible(s, snap) {
			elig = append(elig, s)
		}
	}
	sort.SliceStable(elig, func(i, j int) bool {
		a, b := elig[i], elig[j]
		if a.CacheAge != b.CacheAge {
			return a.CacheAge > b.CacheAge
		}
		if a.ContextPct != b.ContextPct {
			return a.ContextPct > b.ContextPct
		}
		return a.IdleFor > b.IdleFor
	})

	cap := snap.Config.MaxCompactsPerTick
	if cap > 0 && len(elig) > cap {
		elig = elig[:cap]
	}
	return elig
}

// compactReason builds the human-readable rationale attached to a compact
// action and surfaced in the audit log.
func compactReason(s SessionState, level Level) string {
	return "compact idle session at " + level.String() +
		" (ctx=" + itoa(s.ContextPct) + "%, via " + ResolveActuation(s).String() + ")"
}

// itoa avoids pulling strconv into the hot policy path for a single small int.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
