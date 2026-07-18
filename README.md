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
Hook binary for the Codex CLI. Handles `session-start`, `stop`, and `pulse` events,
publishing to nervous-bus and notifying the hearth-loom gateway.

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
