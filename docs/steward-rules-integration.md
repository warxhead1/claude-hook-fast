# Steward Rules Integration — Budget-Aware Fan-Out Gating

**Status: DESIGN PROPOSAL — NOT YET WIRED**

## Overview

The existing `rules` subcommand (wired to `PreToolUse` hook in `tools/claude-hooks/cmd_rules.go`)
can consume the steward's `~/.claude/steward/gate.json` to warn or block new fan-out dispatch
(`Agent`, `Task`, `Workflow` matchers) when budget is under pressure.

## Contract

The steward writes `gate.json` (updated each tick, 30s) with structure:
```json
{
  "state": "NORMAL|WATCH|PRESSURE|CRITICAL",
  "gate": "allow|warn|block",
  "utilization_pct": 75,
  "forecast_exhaust_at": "2026-07-18T14:23:00Z",
  "message": "5h window at 75% / ~18 min to depletion"
}
```

## Proposed Implementation

**Option A** (simpler): Extend `cmd_rules.go` to add a `budget-guard` subcommand that:
1. Reads `gate.json` (returns silently if missing or parse error)
2. On `Agent|Task|Workflow` matcher: check `gate` field
3. Emit `warn` (non-blocking advisory) or `block` (exit 2) accordingly
4. Respect operator override: `test -f ~/.claude/steward/pause && exit 0` (allow if paused)

**Option B**: Add a rules engine `when`-predicate that evaluates `gate.json` inline,
e.g., `when: {file: "~/.claude/steward/gate.json", field: "gate", value: "block"}`.

## Behavior

| gate | Action | Exit Code |
|------|--------|-----------|
| `allow` | emit nothing, proceed | 0 |
| `warn` | emit advisory + budget line to stderr, proceed | 0 |
| `block` | emit block notice to stderr, halt dispatch | 2 |
| missing/error | assume `allow` | 0 |

Pause override: if `~/.claude/steward/pause` exists, treat all gates as `allow`
(operator kill switch takes precedence).

## Notes

- Output format: match existing hook advisory style (prefix `[STEWARD]`, include budget line)
- The rules engine continues to be the single dispatch choke point; steward provides only the budget input file
- Phase 0/1 of rollout require only advisory (`warn`); `block` gates Phase 3
