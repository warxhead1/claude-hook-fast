// Package steward holds the pure, I/O-free policy core of claude-steward:
// the rate-limit-aware session budget manager.
//
// # Purpose
//
// Multiple concurrent Claude Code sessions share one rolling 5-hour
// subscription window. When that window nears exhaustion, sessions die
// mid-flight with large contexts and lingering subagents, leaving them in a
// bad state to resume. claude-steward watches the window and, under pressure,
// compacts idle-but-relevant sessions so they survive the drought in a
// resumable, cache-cheap state, and surfaces lingering subagents.
//
// # Design constraints
//
//   - This package is PURE: Decide(Snapshot) → []Action performs no I/O, reads
//     no clock, touches no filesystem. Every input arrives on the Snapshot;
//     every effect leaves as an Action. This is what makes the entire policy
//     table-driven-testable with zero mocks, and what keeps the Go core and the
//     Python reference (tools/steward.py) provably interchangeable.
//   - Actuation method (tmux vs PTY socket vs none) is resolved by the pure
//     ResolveActuation helper (actuate.go), never inside Decide.
//   - The daemon wiring (readers, actuators, loop) lives in cmd/claude-steward
//     and is the only place that performs I/O.
//
// The rollout ladder (observer → advisory → actuation → hardening) is expressed
// as Config gate booleans, not separate binaries: one binary, phase-enabled by
// flags, so the operator promotes capability incrementally.
package steward

import "time"

// Level is the global budget-pressure state. It rises with 5-hour-window
// utilization (and with a forecast that the window will exhaust before it
// resets) and falls with hysteresis so the steward does not flap at a boundary.
type Level int

const (
	// LevelNormal — observe only; maintain the ledger and budget.json.
	LevelNormal Level = iota
	// LevelWatch — notify once; annotate the budget state.
	LevelWatch
	// LevelPressure — compact eligible idle sessions; report lingering
	// subagents; warn on new fan-out.
	LevelPressure
	// LevelCritical — compact all eligible; block excess fan-out.
	LevelCritical
)

// String renders the level as the lowercase token used in budget.json and logs.
func (l Level) String() string {
	switch l {
	case LevelNormal:
		return "normal"
	case LevelWatch:
		return "watch"
	case LevelPressure:
		return "pressure"
	case LevelCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// SessionKind distinguishes how a session can be reached for actuation. It is
// the load-bearing distinction from the target matrix: an interactive session
// lives in a tmux pane (send-keys), a background/forked session has a daemon
// PTY socket but NO pane (socket is the only push path), and an in-process
// subagent has neither (drive the parent instead).
type SessionKind int

const (
	// KindInteractive — a normal `claude` in a tmux/zellij pane.
	KindInteractive SessionKind = iota
	// KindBackground — a daemon-dispatched worker (--bg/--fork/--batch) with a
	// roster ptySock and no pane.
	KindBackground
)

// Window is one rate-limit window's live state (five-hour or seven-day).
type Window struct {
	// Utilization is 0–100 percent consumed. Anthropic reports integer
	// granularity; forecasting between refreshes uses BurnPctPerMin.
	Utilization float64
	// ResetsAt is when the window rolls over. Zero means unknown/unavailable
	// (the field is absent until the first API response of a session).
	ResetsAt time.Time
	// Present is false when the window was absent from the usage payload.
	Present bool
}

// SessionState is one actuable session's snapshot, assembled by the readers
// from ~/.claude/sessions/<pid>.json, the statusline context files, and the
// daemon roster. All durations are computed against Snapshot.Now by the reader.
type SessionState struct {
	ID   string
	Name string
	Kind SessionKind

	// Status is Claude Code's own registry value: "busy", "idle", or "waiting".
	// Only "idle" is ever a compaction target — "busy" is mid-turn and
	// "waiting" has a permission prompt up that injected keys would answer.
	Status string

	ContextPct      int           // 0–100 of the model context window used
	IdleFor         time.Duration // since the last turn ended
	CacheAge        time.Duration // since the last API call primed the prompt cache
	SubagentsActive int           // live child subagents under this session

	PaneID       string // "tmux:%N" / "zellij:s:p", or "" if none
	PtySock      string // roster ptySock path, or "" if not a daemon worker
	PtyAuthAvail bool   // the roster ptyAuth secret is readable to us

	LastCompact time.Time // last time the steward compacted this session (zero = never)
	Ignored     bool      // per-session opt-out (~/.claude/steward/ignore/<id>)
}

// SubagentState tracks one native subagent's lifecycle for the lingering
// report. Subagents are never compaction targets and cannot be actuated
// externally — the steward can only surface them and exclude their parent from
// compaction while they run.
type SubagentState struct {
	ID       string
	ParentID string
	Name     string
	Started  bool
	Stopped  bool
	// StaleFor is how long since the subagent's transcript mtime last advanced;
	// a long stall on a started-but-not-stopped subagent is "lingering".
	StaleFor time.Duration
}

// Lingering reports whether this subagent is started, not stopped, and stalled
// past the supplied threshold.
func (s SubagentState) Lingering(threshold time.Duration) bool {
	return s.Started && !s.Stopped && s.StaleFor > threshold
}

// Config carries the phase gates and policy thresholds. Defaults live in
// DefaultConfig; the daemon overlays flags and the pause/ignore files.
type Config struct {
	// Phase gates — the rollout ladder. Each higher gate presupposes the lower
	// ones are on, but Decide does not enforce that ordering; the daemon does.
	EnableNotify      bool // Phase 0/1: emit Notify actions
	EnableRulesGate   bool // Phase 1: publish budget.json for the rules engine
	EnableCompact     bool // Phase 2: emit Compact actions
	EnableFanoutBlock bool // Phase 3: escalate the rules gate to block at CRITICAL

	// Level thresholds (percent utilization of the five-hour window).
	WatchPct    float64
	PressurePct float64
	CriticalPct float64
	Hysteresis  float64 // points of overshoot required to step DOWN a level

	// Compaction eligibility.
	MinContextPct      int           // don't bother below this context usage
	CompactCooldown    time.Duration // min gap between compacts of one session
	MinResetHeadroom   time.Duration // if the window resets sooner, do nothing
	MinIdleForCompact  time.Duration // never compact a just-active session
	CacheExpiryHorizon time.Duration // prefer sessions whose cache is near expiry

	// LingeringSubagentAfter is how long a stalled started subagent must be
	// silent before it is reported.
	LingeringSubagentAfter time.Duration

	// Paused halts all ACTUATION (compact + fan-out block) while still
	// computing level and writing budget.json. Set from the pause file.
	Paused bool

	// MaxCompactsPerTick bounds how many sessions the steward compacts in a
	// single tick under CRITICAL, so a burst can't stampede send-keys.
	MaxCompactsPerTick int
}

// DefaultConfig returns the Phase-0 (observer-only) defaults: it observes,
// computes level, and writes budget.json + notifications, but performs no
// compaction and no fan-out gating. Promotion is opt-in via the Enable* gates.
func DefaultConfig() Config {
	return Config{
		EnableNotify:      true,
		EnableRulesGate:   false,
		EnableCompact:     false,
		EnableFanoutBlock: false,

		WatchPct:    60,
		PressurePct: 75,
		CriticalPct: 90,
		Hysteresis:  5,

		MinContextPct:      55,
		CompactCooldown:    30 * time.Minute,
		MinResetHeadroom:   20 * time.Minute,
		MinIdleForCompact:  2 * time.Minute,
		CacheExpiryHorizon: 55 * time.Minute,

		LingeringSubagentAfter: 10 * time.Minute,

		Paused:             false,
		MaxCompactsPerTick: 3,
	}
}

// Snapshot is the complete, pure input to Decide for one tick.
type Snapshot struct {
	Now           time.Time
	FiveHour      Window
	SevenDay      Window
	BurnPctPerMin float64 // EMA of five-hour utilization rate; 0 if unknown
	Sessions      []SessionState
	Subagents     []SubagentState
	PrevLevel     Level // last tick's level, for hysteresis
	Config        Config
}

// TimeToReset returns how long until the five-hour window resets, or 0 if the
// reset time is unknown or already past.
func (s Snapshot) TimeToReset() time.Duration {
	if s.FiveHour.ResetsAt.IsZero() {
		return 0
	}
	d := s.FiveHour.ResetsAt.Sub(s.Now)
	if d < 0 {
		return 0
	}
	return d
}

// ForecastExhaust returns the projected time until the five-hour window is
// fully consumed at the current burn rate, or 0 if burn is unknown/zero.
func (s Snapshot) ForecastExhaust() time.Duration {
	if s.BurnPctPerMin <= 0 {
		return 0
	}
	remaining := 100 - s.FiveHour.Utilization
	if remaining <= 0 {
		return 0
	}
	return time.Duration(remaining/s.BurnPctPerMin*float64(time.Minute))
}

// ActionKind enumerates the effects Decide can request. The daemon's executor
// is the only thing that turns these into I/O.
type ActionKind int

const (
	// ActionSetBudget writes/refreshes the public budget.json contract.
	ActionSetBudget ActionKind = iota
	// ActionNotify surfaces a human-facing message (tmux window + bus).
	ActionNotify
	// ActionCompact requests compaction of one session via its resolved method.
	ActionCompact
	// ActionReportSubagent surfaces a lingering subagent.
	ActionReportSubagent
	// ActionSetFanoutGate updates the rules-engine gate: "warn" or "block".
	ActionSetFanoutGate
)

// FanoutGate is the value an ActionSetFanoutGate carries for the rules engine.
type FanoutGate string

const (
	FanoutAllow FanoutGate = "allow"
	FanoutWarn  FanoutGate = "warn"
	FanoutBlock FanoutGate = "block"
)

// Action is one requested effect. Fields are populated per Kind; unused fields
// stay zero. The executor resolves the concrete mechanism from SessionID +
// Method at I/O time.
type Action struct {
	Kind      ActionKind
	Level     Level
	SessionID string     // ActionCompact / ActionReportSubagent (parent)
	Method    Method     // ActionCompact: how to reach the session
	Gate      FanoutGate // ActionSetFanoutGate
	Reason    string     // human-readable rationale, logged and surfaced
}
