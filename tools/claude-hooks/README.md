# Claude Hook Fast

A high-performance Go binary that replaces slow Node.js Claude Code hooks.

## Overview

Every Claude Code tool call triggers ~4 Node.js hook scripts. Each Node cold-start is 500ms–2s; with 11+ `require()` calls per script they frequently SIGKILL at their 1–2s timeout. This binary eliminates that overhead by compiling the hot-path hooks to a single Go binary with <50ms cold-start per invocation.

## Hooks Replaced

| Hook | JS File | Timeout | Kill Rate |
|------|---------|---------|-----------|
| tmux-preparing | `pre-tool-use/tmux-preparing.js` | 2s | High |
| plan-tracker | `post-tool-use/plan-tracker.js` | - | - |
| agent-heartbeat | `post-tool-use/agent-heartbeat.js` | 1s | 1.5% |
| feedback-collector | `post-tool-use/zzzz-final-feedback-collector.js` | 9s | 166 kills |
| tmux-ready | `stop/tmux-ready.js` | - | - |
| tmux-notification | `notification/tmux-notification.js` | - | - |
| tmux-thinking | `user-prompt-submit/tmux-thinking.js` | - | - |

## Building

```bash
go build -trimpath -ldflags='-s -w' -o bin/claude-hook-fast ./tools/claude-hooks
```

## Installation

1. Copy the binary to your path:
```bash
cp bin/claude-hook-fast ~/.local/bin/
chmod +x ~/.local/bin/claude-hook-fast
```

2. Edit `~/.claude/settings.json` to swap one hook at a time. Example diff:
```json
{
  "hooks": {
    "pre-tool-use": {
      "tmux-preparing": "claude-hook-fast tmux-preparing"
    }
  }
}
```

3. Start with `tmux-preparing` (highest fail rate), observe `/tmp/hook-timeout-wrapper.log` for a session, then swap the next hook.

## Usage

```bash
claude-hook-fast <subcommand>
```

Subcommands:
- `tmux-preparing` - Pre-tool hook for tmux status
- `plan-tracker` - Post-tool hook for ExitPlanMode tracking
- `agent-heartbeat` - Post-tool heartbeat to MCP
- `feedback-collector` - Final feedback collection hook
- `tmux-ready` - Stop event hook
- `tmux-notification` - Notification event hook
- `tmux-thinking` - User prompt submit hook

Unknown subcommand exits with code 2 and `unknown subcommand` on stderr.

## Binary Contract

Invoked as: `claude-hook-fast <subcommand>` with JSON on stdin.

**Stdout parse error → exit 0 silent** (matches JS behavior — hooks must never block tool calls).

## Rollout Plan

1. Copy binary to `~/.local/bin/`
2. User edits `~/.claude/settings.json` to swap ONE hook at a time
3. Observe `/tmp/hook-timeout-wrapper.log` for a session; if the new binary shows COMPLETED with small elapsed, swap the next hook
4. After all 7 hot-path hooks are switched, JS versions can stay in repo as reference but are no longer invoked

## What This Does NOT Replace (Phase 2)

- `post-tool-use/shader-validator.js` (40KB, tengine-specific)
- `post-tool-use/brain-telemetry.js` (SQLite brain DB)
- `post-tool-use/hardcoded-constants-guard.js`
- `post-tool-use/shader-hardcoded-ids-guard.js`
- `session-start/*` (runs once per session, cost amortized)
- Achievement system files (disabled)

## Performance

- Cold start per invocation (no warmup): <50ms on typical hardware
- Zero external syscalls beyond what the JS version makes
- Zero network calls during normal operation
