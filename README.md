# claude-hook-fast

Fast Go hook binaries for Claude Code, Codex, and hearth-loom container runtimes.
Extracted from hearth-loom to be standalone and independently installable.

## Binaries

### `claude-hook-fast` (`tools/claude-hooks/`)
The flagship hook binary for Claude Code sessions. Multi-subcommand dispatcher replacing
slow Node.js/bash hooks to eliminate cold-start latency under loomie load.

Subcommands: `tmux-preparing`, `tmux-ready`, `tmux-notification`, `tmux-thinking`,
`plan-tracker`, `agent-heartbeat`, `feedback-collector`, `post-hearth-loom`,
`nbus-activity`, `nbus-session-start`, `nbus-stop`, `nbus-notification`,
`nbus-pre-compact`, `nbus-permission-request`, `nbus-message-display`,
`nbus-stop-failure`, `nbus-session-end`, `nbus-subagent-lifecycle`,
`register-with-hearth-loom`, `deregister`, `steering-poll`,
`cleanup-tmp`, `hardcoded-constants-guard`, `shader-hardcoded-ids-guard`,
`shader-compile-validator`, `flow`, `rules`

### `codex-hook` (`cmd/codex-hook/`)
Hook binary for Codex 0.149. It supports `SessionStart`, `Stop` (a `turn_end`,
not a terminal session), `SessionEnd`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `PermissionRequest`, `PreCompact`, `PostCompact`, `SubagentStart`,
and `SubagentStop`. Copy [configs/codex-hooks.toml.example](configs/codex-hooks.toml.example)
into `~/.codex/config.toml`; keep the single absolute binary path in every block.

Codex hook input is fail-open. The producer records the root `session_id`, a
qualified `tmux:%N` or `zellij:<session>:<pane>` binding, and explicit child
`agent_id`/`agent_type`; subagents never claim the parent pane. Tool payloads are
redacted and bounded. A single detached, PID-starttime-validated lease worker
refreshes the root binding about every 30 seconds and exits on terminal cleanup,
stale parent, or six minutes without a hook refresh. Both hook binaries append
a buffered debug receipt before starting a detached bus worker. The worker
inherits that open file, fsyncs it, and sends the exact recorded envelope to
Redis. Rotation cannot make it sync a different file, and the receipt and Redis
event share one ID and timestamp even when the payload has no `event_id`.

Hook return acknowledges the append and worker launch, **not durable storage or
delivery**. A host crash before the worker syncs can lose the buffered receipt.
The mirror may also observe the append before that sync; this is a best-effort
telemetry backstop, not an acknowledged outbox. Receipt or worker-start errors
are logged without synchronous network/CLI fallback; a failed worker sync never
sends to Redis. A Redis failure leaves the synced receipt for the mirror.
This keeps slow disk flushes outside the interactive hook's timeout budget.
The six-minute timeout only bounds an idle lease worker; it is not evidence that a
Codex session ended. The deterministic unit test bounds in-process work, while
release validation should also use a real subprocess measurement (the current
50-hook historical host measurement was about 3.02 ms average; repeat it under
current host load rather than treating it as a latency guarantee).

Claude's `steering-poll` uses one 500 ms deadline for both response headers and
body, leaving headroom inside its configured 1 s hook timeout. An incomplete
response never advances the steering cursor.

`PreToolUse` additionally enforces the versioned worker-model policy in
[`internal/hooks/codexpolicy/worker-policy.json`](internal/hooks/codexpolicy/worker-policy.json).
Every Orca worker launch must name its agent; Codex workers must name exactly one
allowed model. Luna and Terra are directly allowed. Sol remains available for
exceptional work but requires the additional explicit launch prefix
`CODEX_ALLOW_SOL_WORKER=1`. Astra workers likewise require
`CODEX_ALLOW_ASTRA_WORKER=1`; this restriction applies to supervised Orca worker
launches, not ordinary primary Codex sessions. Unknown models, missing models,
duplicate model flags, and unacknowledged Astra or Sol launches are denied using
Codex's native `permissionDecision: deny` response. A missing installed policy uses the embedded
restrictive default; a malformed installed policy fails closed.

The managed host default is `gpt-6-astra`. The active agent implements and
validates work directly; independent Luna reviews and bounded MMX/Terra slices
are optional when their benefit outweighs handoff and review cost. Testability
or a cheaper available model alone is not a reason to delegate implementation.

Install or audit the host-wide Astra default, managed `~/.codex/AGENTS.md` policy,
data-driven worker allowlist, and enforcing hook with:

```bash
make install-codex-host-policy
make check-codex-host-policy
```

The installer preserves unrelated config and AGENTS content, migrates the prior
unmarked delegation contract, writes backups before replacement, updates files
atomically, and validates the resulting Codex config in the Make target.

### `hearth-loom-hook` (`cmd/hearth-loom-hook/`)
In-container HTTP-push observability shim used inside hearth-loom dispatch containers.
Handles Claude Code and opencode hook events, POSTing to the hearth-loom gateway.

### `claude-steward` (`cmd/claude-steward/`)
Rate-limit-aware session budget steward. Watches the 5-hour subscription window,
tracks per-session context utilization and burn rate, and compacts idle-but-relevant
sessions under budget pressure so they survive rate-limit droughts in a resumable,
cache-cheap state. Also surfaces lingering subagents to the operator for triage.

Phases (opt-in via flags on one binary):
- **Phase 0** (observer): ledger + budget metrics only, no actuation
- **Phase 1** (advisory): rules-engine warnings on fan-out under pressure
- **Phase 2** (actuation): compacts idle sessions via tmux; gating on new dispatch
- **Phase 3** (hardening): blocks fan-out in CRITICAL state; PTY socket fallback

Key files:
- `~/.claude/steward/budget.json` — public contract: utilization, burn rate, per-session context, eligibility
- `~/.claude/steward/gate.json` — rules engine input: budget state for `Agent|Task|Workflow` gating
- `~/.claude/steward/actions.jsonl` — audit log of all actuation decisions
- `~/.claude/steward/pause` — kill switch: `touch` to halt all actuation
- `~/.claude/steward/ignore/<session_id>` — per-session opt-out from compaction

Reference implementation: `tools/steward.py` (stdlib Python, ~300 lines; policy prototyping).
Test: `python3 tools/steward.py --selftest`.

Build and install: `make install-steward`.

See `docs/design-budget-steward.md` for the full design, signal inventory, policy, and rollout ladder.

## Build

```bash
# All binaries
go build ./...

# Flagship (release strip)
go build -trimpath -ldflags='-s -w' -o /tmp/claude-hook-fast ./tools/claude-hooks/

# Other binaries
go build -trimpath -ldflags='-s -w' -o /tmp/codex-hook ./cmd/codex-hook/
go build -trimpath -ldflags='-s -w' -o /tmp/hearth-loom-hook ./cmd/hearth-loom-hook/
```

## Install

```bash
# Install claude-hook-fast to ~/.local/bin
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/claude-hook-fast ./tools/claude-hooks/

# Install codex-hook to PATH
go build -trimpath -ldflags='-s -w' -o ~/.local/bin/codex-hook ./cmd/codex-hook/
```

## Tests

```bash
go test ./...
```

## Shared packages

The `internal/hooks/` packages are shared across all three binaries:

- `internal/hooks/client` — keepalive HTTP client tuned for hook latency
- `internal/hooks/event` — Claude Code / opencode stdin JSON parsing
- `internal/hooks/hooklog` — fire-and-forget diagnostic log writer
- `internal/hooks/nbuspub` — shells out to `nervous publish` for bus events
- `internal/hooks/project` — CWD → canonical (project, worktree) resolver

**Note**: `internal/hooks/nbuspub` and `internal/hooks/project` are also imported by
the hearth-loom main binary (`cmd/hearth-loom/triage.go` and `cmd/hearth-loom/codex_hook.go`).
This means hearth-loom currently contains COPIES of those packages. A future cleanup
could invert the dependency (hearth-loom imports this module), but that requires a
published release or local `replace` directive.

## Requirements

- Go 1.26+
- `nervous` CLI on PATH (for nbus-* subcommands; silently no-ops if absent)
- hearth-loom gateway at `$HEARTH_LOOM_GATEWAY_URL` or `http://127.0.0.1:5908`
  (for `hearth-loom-hook` and `codex-hook`; silently no-ops if unreachable)
