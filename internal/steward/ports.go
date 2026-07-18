package steward

import (
	"context"
	"time"
)

// This file defines the ports (interfaces) between the pure policy core and the
// I/O edges, plus the public budget.json document schema. Concrete
// implementations live in cmd/claude-steward (Go) and tools/steward.py (the
// interchangeable Python reference). Keeping the interfaces and the document
// schema in the pure package is what lets every edge component be built and
// tested independently against a frozen contract.

// Reader assembles a Snapshot of the world for one tick. Implementations read
// the usage cache, ~/.claude/sessions/<pid>.json, the statusline context files,
// the daemon roster, and subagent transcripts. A Reader performs I/O and may
// return a partial Snapshot with a non-nil error; the daemon decides whether a
// partial read is still actionable (a missing usage file, for instance, means
// "observe only").
type Reader interface {
	Read(ctx context.Context, now time.Time, prev Level, cfg Config) (Snapshot, error)
}

// Actuator executes a compaction against one session using the pre-resolved
// Method. Compact MUST honor the send-keys safety protocol (idle-only, pane
// command verification, audit-before-send, no retry loop) for MethodTmux, and
// the framed-socket handshake for MethodPtySock. It returns an error on any
// pre-flight failure so the daemon can mark the session do-not-touch.
type Actuator interface {
	Compact(ctx context.Context, s SessionState, method Method, reason string) error
}

// Notifier surfaces a human-facing message (tmux window text + bus event).
type Notifier interface {
	Notify(ctx context.Context, level Level, sessionID, message string) error
}

// BudgetWriter persists the public budget.json contract atomically.
type BudgetWriter interface {
	Write(ctx context.Context, doc BudgetDoc) error
}

// GateWriter publishes the fan-out gate the rules engine reads to warn/block
// new Agent|Task|Workflow dispatch under pressure.
type GateWriter interface {
	SetGate(ctx context.Context, gate FanoutGate, level Level) error
}

// BudgetDoc is the JSON contract at ~/.claude/steward/budget.json. It is the
// single public interface other tools (the rules engine, dashboards) consume.
// Time fields are RFC3339 strings; empty means unknown/absent.
type BudgetDoc struct {
	State         string        `json:"state"`
	UpdatedAt     string        `json:"updated_at"`
	FiveHour      WindowDoc     `json:"five_hour"`
	SevenDay      WindowDoc     `json:"seven_day"`
	BurnPctPerMin float64       `json:"burn_rate_pct_per_min"`
	ForecastAt    string        `json:"forecast_exhaust_at,omitempty"`
	FanoutGate    string        `json:"fanout_gate"`
	Sessions      []SessionDoc  `json:"sessions"`
	Subagents     []SubagentDoc `json:"lingering_subagents,omitempty"`
}

// WindowDoc mirrors a rate-limit window in budget.json.
type WindowDoc struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at,omitempty"`
	Present     bool    `json:"present"`
}

// SessionDoc is one session's public row in budget.json.
type SessionDoc struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	ContextPct      int    `json:"context_pct"`
	IdleForS        int    `json:"idle_for_s"`
	CacheAgeS       int    `json:"cache_age_s"`
	SubagentsActive int    `json:"subagents_active"`
	Method          string `json:"actuation_method"`
	Eligible        bool   `json:"compact_eligible"`
}

// SubagentDoc is one lingering subagent's public row.
type SubagentDoc struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	StaleForS int   `json:"stale_for_s"`
}

// BuildBudgetDoc renders a Snapshot + resolved level/gate into the public
// document. It is pure so the schema construction is testable without I/O; the
// BudgetWriter merely marshals and atomically writes the result. nowRFC3339 is
// supplied by the caller (the core never reads the clock).
func BuildBudgetDoc(snap Snapshot, level Level, gate FanoutGate, nowRFC3339 string) BudgetDoc {
	doc := BudgetDoc{
		State:         level.String(),
		UpdatedAt:     nowRFC3339,
		FiveHour:      windowDoc(snap.FiveHour),
		SevenDay:      windowDoc(snap.SevenDay),
		BurnPctPerMin: snap.BurnPctPerMin,
		FanoutGate:    string(gate),
	}
	if fe := snap.ForecastExhaust(); fe > 0 {
		doc.ForecastAt = snap.Now.Add(fe).UTC().Format(time.RFC3339)
	}
	for _, s := range snap.Sessions {
		doc.Sessions = append(doc.Sessions, SessionDoc{
			ID:              s.ID,
			Name:            s.Name,
			Kind:            kindString(s.Kind),
			Status:          s.Status,
			ContextPct:      s.ContextPct,
			IdleForS:        int(s.IdleFor.Seconds()),
			CacheAgeS:       int(s.CacheAge.Seconds()),
			SubagentsActive: s.SubagentsActive,
			Method:          ResolveActuation(s).String(),
			Eligible:        compactEligible(s, snap),
		})
	}
	for _, sub := range snap.Subagents {
		if sub.Lingering(snap.Config.LingeringSubagentAfter) {
			doc.Subagents = append(doc.Subagents, SubagentDoc{
				ID:        sub.ID,
				ParentID:  sub.ParentID,
				Name:      sub.Name,
				StaleForS: int(sub.StaleFor.Seconds()),
			})
		}
	}
	return doc
}

func windowDoc(w Window) WindowDoc {
	d := WindowDoc{Utilization: w.Utilization, Present: w.Present}
	if !w.ResetsAt.IsZero() {
		d.ResetsAt = w.ResetsAt.UTC().Format(time.RFC3339)
	}
	return d
}

func kindString(k SessionKind) string {
	switch k {
	case KindBackground:
		return "background"
	default:
		return "interactive"
	}
}
