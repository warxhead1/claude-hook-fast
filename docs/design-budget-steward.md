# Budget Steward — rate-limit-aware session compaction & subagent triage sidecar

Status: DESIGN (2026-07-17). No code yet. Research basis: repo substrate map,
live `~/.claude` runtime inventory, and docs verification (hooks.md,
statusline.md, commands.md, sub-agents.md, prompt-caching.md, costs.md).

## Problem

Multiple concurrent Claude Code sessions share one 5-hour subscription window.
When the window nears exhaustion, sessions die mid-flight with large contexts
and lingering subagents, leaving them in a bad state to resume. Nothing today
watches the window and acts. The steward is a slim sidecar daemon that:

1. Tracks 5h-window utilization and per-session burn rate.
2. When budget pressure crosses thresholds, compacts idle-but-relevant
   sessions (so they survive the drought in a resumable, cache-cheap state).
3. Surfaces lingering subagents to the operator and to the parent session.
4. Dampens new fan-out (Agent/Task/Workflow dispatch) when budget is critical.

## Non-goals

- Do NOT keep caches warm by pinging sessions. A keep-warm request is a real
  API call and consumes the exact 5h budget the steward protects. Never.
- Do NOT kill in-session subagents externally — there is no supported API for
  it (`claude agents --json` only sees background sessions; in-session tasks
  are `/tasks`-interactive-only). Detection + surfacing only.
- Do NOT re-add context% math into the hook binary — that was deliberately
  removed (b5a5e0b, "R6: obsoleted by hearth's server-side token computation").
  The steward computes independently from files/bus.

## Signal inventory (all verified live on this box)

| Signal | Source | Freshness | Authority |
|---|---|---|---|
| 5h/7d utilization + resets_at | `/tmp/claude-usage-cache.json` (statusline.js curls `api.anthropic.com/api/oauth/usage`, 120s TTL cache) | 120s | Endpoint is community-reverse-engineered; the OFFICIAL equivalent is the statusline stdin `rate_limits.five_hour.{used_percentage,resets_at}` payload |
| Per-session context % + pane | `$XDG_RUNTIME_DIR/hearth/context/claude-context-<sid>.json` and `/tmp/claude-context-cli_<sid>.json` — `{currentContext, tmux_pane, pid, is_subagent}` (written by statusline.js each render; subagents deliberately null pane/pid) | per render | derived from official statusline `context_window.remaining_percentage` |
| Session liveness + busy/idle/waiting | `~/.claude/sessions/<pid>.json` — `{pid, sessionId, cwd, name, status: busy\|idle\|waiting, updatedAt, version, kind}` | live | Claude Code's own registry; verify pid against `/proc` |
| Per-call token usage + model | nervous-bus `bus.agent.activity.v1` (input/output/cache_read/cache_write tokens, model, tool_use_id) — emitted by `nbus-activity` PostToolUse hook; or tail transcripts via `internal/hooks/transcript` | per tool call | transcript ground truth |
| Subagent lifecycle | Subagent transcripts at `~/.claude/projects/<slug>/<sid>/subagents/agent-*.jsonl` (+ `.meta.json` carrying `toolUseId`, write-once, no status field). Completion is read from the **parent** transcript's `tool_result` for that `toolUseId`, NOT from the `SubagentStart`/`SubagentStop` hooks (which cannot identify an individual subagent — see below). jsonl mtime is the staleness heartbeat. | transcript pairing + mtime | parent `tool_result` authoritative for completion; mtime for staleness |
| Turn boundaries | `Stop` hook / `agent.session.v1` turn_end (already emitted with pane_id) | per turn | official hook |
| Autocompact levers | `settings.json` env: `CLAUDE_CODE_AUTO_COMPACT_WINDOW=375000`, `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80` | static | user-configured |

Hook stdin schemas carry NO token/rate-limit fields (confirmed against
hooks.md; open upstream FR anthropics/claude-code#55333 to expose
`anthropic-ratelimit-unified-5h-*` to hooks). Everything must come from the
files above. If upstream ever ships #55333, swap the usage source and delete
the curl fallback — nothing else changes.

## Actuation inventory

| Action | Mechanism | Constraints |
|---|---|---|
| Trigger `/compact` in a live session | `tmux send-keys -t <pane> "/compact" Enter` | ONLY path that works. `/compact` is NOT honored in `-p`/print mode; SDK has no `compact()`; PreCompact hook is reactive-only. `--resume` from a second process interleaves transcripts — never use it for control. |
| Dampen fan-out | data-driven rule in `~/.claude/hook-rules.json` (existing `rules` engine, PreToolUse `Agent\|Task\|Workflow` matcher) reading a steward-written budget state file — `warn` at high, `block` at critical | needs one small rules-engine extension: a `when` predicate over an external JSON state file |
| Notify operator | existing `tmux-notification` path / `nervous publish` / tmux window rename | already built |
| Inject advisory into a session | hook `additionalContext` on SessionStart(source=compact) — already exists (`emitCompactDispatchState`); extend with budget line | advisory only (hard-gating was tried and deliberately reverted in d21e512) |
| (Experimental) keystroke injection without tmux | `~/.claude/daemon/roster.json` → per-session `ptySock` under `/tmp/cc-daemon-1000/` | undocumented protocol, now decoded (see below); Phase-3, not the design basis — and auth-gated in practice |

### bg-pty-host wire protocol — DECODED AND LIVE-VALIDATED (CLI 2.1.212, 2026-07-18)

The daemon roster's `ptySock` speaks a length-prefixed framed protocol. The
`bg-pty-host` worker process (`claude bg-pty-host <sock> <cols> <rows> -- ...`)
is the SERVER owning the real claude PTY; the supervisor connects as CLIENT.

- **Frame** = 5-byte header + payload. Header: `uint32be` payload length (bytes
  0–3), `uint8` kind (byte 4). Max frame 1 MiB.
- **kind 0** = raw PTY bytes (keystrokes toward claude / terminal output back).
- **kind 1** = control JSON frame. Verbs seen: `ping`/`pong`, `auth`
  (`{t:"auth",token}`), `auth-required`, `signal`.
- **Client handshake on connect**: send ctrl `{t:"pong"}`, then
  `{t:"auth",token:<ptyAuth>}`, then stream keystrokes as kind-0 frames.
- **Server attach sequence** (observed): ctrl `{t:"hello"}` → a burst of kind-0
  frames replaying the current screen/scrollback (~15 KB) → ctrl `{t:"live"}` →
  periodic ctrl `{t:"ping"}`. **The client MUST reply `{t:"pong"}` to each ping**
  or the host drops it. This is exactly what `claude attach` does internally.
- **`ptyAuth`** (and `rvAuth`) are per-worker 16-byte hex secrets in
  `roster.json`, rotated on rekey.

**Live-validation results (disposable haiku `--bg` worker, 2026-07-18):**
- A second client attaching with the correct `ptyAuth` is **cleanly accepted** —
  no auth poisoning, no eviction of the real supervisor. Poisoning is the
  *wrong-token* path, not the concurrent-attach path.
- Keystrokes land end-to-end: a typed marker echoed back inside the TUI's
  cursor-positioning escapes (input box populated, not executed without `\r`).
- `/compact\r` executes as a **real slash command** — output showed
  `Running PreCompact hooks……` then `Not enough messages to compact` (correct
  no-op on an empty session). This is the genuine command path, not literal text.
- **Keystroke→echo round-trip: ~32 ms** (includes full TUI redraw).

**Cadence comparison (the operator's question):**

| Path | Direction | Latency | Reaches an IDLE session? | Coverage |
|---|---|---|---|---|
| PTY socket inject | push | ~32 ms | **Yes** | daemon `--bg`/`/fork`/`/batch` workers only |
| tmux `send-keys` | push | ~5–15 ms (process spawn) | **Yes** | ALL interactive tmux-paned sessions |
| hearth-api steering (`GET /sessions/{id}/commands?since=`) | **pull** | bounded by next `PreToolUse` | **No** — idle session runs no tools, never polls | any hooked session |
| hearth-api HTTP (`/health` baseline) | req/resp | ~4–22 ms loopback | n/a | n/a |

The load-bearing distinction is **push vs pull**. hearth-api's existing steering
is pull-based and gated on the session's next tool call — so it structurally
**cannot drive a session sitting idle at the prompt**, which is exactly the
steward's compaction target. Both push paths (PTY socket, tmux) can. All four
are sub-100 ms and utterly dominated by the steward's 30 s tick, so cadence is
not the deciding factor — reachability-while-idle is.

**Why tmux still leads despite the PTY socket working:**
1. **Coverage.** `ptySock` exists only for daemon-dispatched workers, not for a
   plain interactive `claude` in a tmux pane — the steward's common target. tmux
   `send-keys` covers all interactive sessions; the socket covers a subset.
2. **Destructive failure exists on the wrong-token path.** The binary logs,
   verbatim: *"host rejected auth token — roster ptyAuth poisoned; input is dead
   until the worker is re-keyed"* (exec workers never auto-respawn). Correct-token
   attach is clean (validated), but any token-rotation race turns a benign
   compaction into a bricked input channel — a failure mode tmux does not have.
3. **Undocumented + secret-gated.** The protocol is reverse-engineered and can
   change across CLI versions; `ptyAuth` reads and daemon-socket connects require
   an explicit operator Bash-permission grant. A feature (can't be enabled
   silently), but operational friction.

Net: tmux `send-keys` remains the foundation. The PTY socket is now an
*empirically validated, accurately characterized* alternative — genuinely useful
for the daemon-worker subset (headless `--bg` sessions with no tmux pane, where
it's the ONLY push path) — offered behind an explicit permission grant. Use it
as a **fallback keyed on session kind**: tmux for paned interactive sessions,
PTY socket for daemon workers that have a `ptySock` but no pane.

### send-keys safety protocol (the part that must be airtight)

1. Resolve pane from the context file for that session_id; ignore entries with
   `is_subagent` or null pane.
2. Cross-check `~/.claude/sessions/<pid>.json`: pid alive in `/proc`,
   `status == "idle"`. Never `busy` (mid-turn), never `waiting` (a permission
   prompt is up — injected keys would ANSWER THE PROMPT).
3. Verify the pane is actually running claude:
   `tmux display -p -t %N '#{pane_current_command}'` must be the claude
   process (not a shell, not vim).
4. Re-read status immediately before send; send `/compact` + Enter as one
   atomic send-keys; write an action record (JSONL) with session, pane,
   utilization, context%, reason BEFORE sending.
5. Post-verify: within 60s expect SessionStart(source=compact) via the bus (or
   context file % drop). No confirmation → mark the session do-not-touch and
   notify the operator; never retry-loop keystrokes into a pane.

## Actuation target matrix — sessions vs subagents (empirically verified 2026-07-18)

The unit of actuation is the **session**, never the in-process subagent. Verified
by spawning a live subagent and observing zero new process / zero roster entry /
zero socket — an Agent-tool subagent (even worktree-isolated) is an async task
inside the orchestrator's single process, with no PTY of its own.

| Target | tmux pane? | `ptySock`? | Push actuation | Notes |
|---|---|---|---|---|
| Interactive session in a tmux pane | yes | no | **tmux `send-keys`** | the common case |
| Background/forked session (`--bg`/`/fork`/`/batch`) | **no** | **yes** | **PTY socket (only option)** | no pane exists → tmux can't reach it; socket is the sole push path |
| In-process subagent / teammate (Agent tool) | no (shares parent's) | **no** | **none direct** | drive the PARENT session; orchestrator owns child lifecycle |

Consequences for the steward:
- To wind down lingering subagents, the steward **acts on the parent session**
  (inject into its pane/socket, or notify), because the orchestrator is the only
  thing that can stop or wait on its own children. Per-subagent completion comes
  from the parent transcript's `tool_result` (not the lifecycle hooks); there is
  no external per-subagent kill.
- The only per-subagent *steering* channel is hearth-loom's `steeringTargetID()`
  (addresses a subagent by transcript path) — but it is **pull-based**, delivered
  at the subagent's next `PreToolUse`; useless against an idle/blocked subagent.
- The PTY socket's real niche is the **headless background session** with a
  `ptySock` but no pane — there tmux is structurally unavailable and the socket
  is the ONLY push actuator. This is the strongest argument for keeping the
  socket path in the design, distinct from "a tmux alternative."

## Policy

Inputs per tick (30s): `U` = five_hour utilization, `R` = time until
resets_at, per-session `{C: context%, status, idle_for, cache_age, active_subagents}`,
global burn rate `B` (utilization %/min, EMA over the activity token stream —
forecasts exhaustion: `T_exhaust = (100-U)/B`).

States (global): `NORMAL` → `WATCH` (U≥60) → `PRESSURE` (U≥75 or
T_exhaust < R) → `CRITICAL` (U≥90). Hysteresis 5 points on the way down.

| State | Behavior |
|---|---|
| NORMAL | observe, maintain ledger, nothing else |
| WATCH | notify once (tmux + bus); annotate status file |
| PRESSURE | compact-eligible sessions get compacted (see below); lingering-subagent report; rules engine `warn` on new Agent/Task/Workflow dispatch with remaining-budget line |
| CRITICAL | compact all eligible; rules engine `block` on fan-out >1 concurrent (operator override via flag file); proactive notification with per-session triage summary |

Compact-eligible = `status==idle` AND `C ≥ 55%` AND no active subagents AND
not compacted in last 30 min AND `R > 20min` (if the window resets in <20min,
do nothing — the reset solves it for free).

Cache-TTL tiebreak (the "4 sessions, 3 going, all 4 relevant" scenario): main
sessions get the free 1h-TTL cache on subscription; subagents are always
5m-TTL. An idle session whose `cache_age` approaches ~55min is about to lose
its prefix — resuming it later cold-reprimes the full context. Compacting it
*before* expiry reads mostly-cached input (cheap) and leaves a small context
that re-primes cheaply after the drought. So eligibility ranking =
`(closest to cache expiry, largest C, longest idle)` first. Never compact the
session the user is actively typing in (`busy`, or idle_for < 2min).

Lingering subagent = a subagent that has **not completed** AND whose
`subagents/agent-*.jsonl` mtime is stale > 10min. Completion is NOT read from
the `SubagentStart`/`SubagentStop` hooks — those cannot identify an individual
subagent: `subagent_id` is never populated, `SubagentStop` carries only the
parent's session_id + transcript, and start/stop counts do not reconcile
(48 vs 121 in one observed 24h window). The authoritative, retroactively
available completion signal is the `tool_use_id` ↔ `tool_result` pairing: a
dispatched subagent's `.meta.json` carries a `toolUseId`, and its completion is
recorded in the **parent** session transcript (`<slug>/<sid>.jsonl`) as a
`tool_result` bearing that id. The steward tails each parent transcript
incrementally (append-only JSONL; cursor offset + completed-id set persisted to
`~/.claude/steward/completions.json`) and marks a subagent `Stopped` when its
`tool_result` appears. Subagents with no `toolUseId` (in-process teammates) or
whose completion is not yet confirmed fall back to mtime staleness alone.
Steward can't kill a lingerer; it reports (pane notification + bus event +
status file) so the operator or the parent session acts, and it excludes the
parent from compaction while a subagent is genuinely still running (a compact
under a running subagent loses the dispatch state).

## Architecture

```
                      ┌─────────────────────────────┐
 /tmp/claude-usage-   │  claude-steward (daemon)     │   tmux send-keys /compact
 cache.json      ───▶ │  observe → ledger → policy   │ ─▶ (safety protocol)
 sessions/*.json ───▶ │  (pure decide() function)    │ ─▶ ~/.claude/steward/budget.json
 context/*.json  ───▶ │  tick 30s, stateless-restart │ ─▶ nervous publish + tmux notify
 nbus subscribe  ───▶ │                              │ ─▶ actions.jsonl (audit log)
                      └─────────────────────────────┘
```

- **One process, one loop, no new sockets, no new bus.** All inputs are files
  + optional bus subscribe; all outputs are files + shell-outs. Restartable at
  any time: everything is re-derived from disk on start (only cursors and the
  per-session cooldown map persist, in `~/.claude/steward/state.json`).
- **`decide()` is pure**: `Snapshot → []Action`. All I/O lives at the edges.
  This is what makes it promotable — the entire policy is table-driven-testable
  with zero mocks.
- `~/.claude/steward/budget.json` is the public contract consumed by the rules
  engine and anything else:
  `{state, five_hour: {utilization, resets_at}, burn_rate_pct_per_min,
    forecast_exhaust_at, sessions: [{id, name, context_pct, status, idle_for_s,
    cache_age_s, subagents_active, eligible, last_action}], updated_at}`.
- Ships as `cmd/claude-steward/` in this module. Reuses
  `internal/hooks/{transcript,project,nbuspub,hooklog}` directly (same module,
  no replace directive). Follows house style: file-header doc comments with
  design constraints, FAF discipline everywhere except the audit log,
  table-driven tests, `make install` atomic-rename deploy, systemd user unit
  (`Restart=on-failure`) for lifecycle.

### Go vs Python — "interchangeable" is a property of the contract, not the code

Primary implementation: **Go** (matches the repo, imports the existing
packages, single static binary, sub-ms file polls). But every interface is
deliberately language-agnostic — read JSON files, one curl fallback, shell out
to tmux/nervous — so a Python reference (`tools/steward.py`, stdlib-only,
~300 lines) can implement the identical contract for experimentation. The two
stay interchangeable as long as both honor: (1) the input file paths above,
(2) `budget.json` schema, (3) the send-keys safety protocol, (4) single-instance
lock (`~/.claude/steward/lock`). Prototype policy tweaks in Python, promote to
Go when stable.

## Rollout (promotion-comfort ladder)

1. **Phase 0 — observer.** Ledger + `budget.json` + notifications only. No
   send-keys, no rules. Run for days; eyeball `actions.jsonl` "would-compact"
   dry-run records against reality.
2. **Phase 1 — advisory.** Rules-engine `warn` on fan-out under PRESSURE;
   budget line injected post-compact via existing `additionalContext` path.
3. **Phase 2 — actuation.** send-keys `/compact` enabled behind
   `--enable-compact`, idle sessions only, full safety protocol, per-session
   opt-out (`~/.claude/steward/ignore/<session_id>`).
4. **Phase 3 — hardening.** CRITICAL-state fan-out `block`; evaluate the
   daemon ptySock as a tmux-free actuation path; revisit upstream #55333.

Kill switch at every phase: `touch ~/.claude/steward/pause` halts all
actuation on the next tick (checked before every action).

## Open questions

- Rules-engine `when`-predicate-over-external-file extension: small addition to
  `cmd_rules.go`, or a dedicated `budget-guard` subcommand on the
  `Agent|Task|Workflow` matcher (simpler, likely the right call).
- `five_hour.utilization` granularity is integer percent — burn-rate EMA over
  the token stream covers forecasting between 120s cache refreshes; calibrate
  the tokens→percent factor empirically during Phase 0.
- Zellij panes: context files carry `zellij_pane_id`; send-keys equivalent is
  `zellij action write-chars` — Phase 2 stretch, tmux first.
- Whether hearth-loom's server-side context computation should eventually own
  this policy (it already owns context_pct). Steward is deliberately
  standalone-first; if it proves out, it can become a hearth-loom module — the
  file contract makes that migration mechanical.
