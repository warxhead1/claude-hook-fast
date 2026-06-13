# Reference JS sources

Copied from `~/.claude/hooks/` on 2026-04-23 for the Go port (loom-647i).

Use these as the authoritative behavior spec. The Go binary must produce the same observable side effects (tmux status writes, session state JSON under `~/.claude/hooks/data/`, log lines).

When porting, do NOT copy this directory into the final binary. It's reference material only. After the PR merges, this dir can stay (for future ports of phase-2 hooks) or be removed — operator's call.

## Layout

```
reference-js/
├── pre-tool-use/
│   └── tmux-preparing.js       # runs on every tool call
├── post-tool-use/
│   ├── plan-tracker.js          # tracks Plan tool invocations
│   ├── agent-heartbeat.js       # pings agent-activity daemon
│   └── zzzz-final-feedback-collector.js  # aggregates feedback at turn end
├── stop/
│   └── tmux-ready.js            # marks tmux "ready for prompt"
├── notification/
│   └── tmux-notification.js     # notification → tmux status
├── user-prompt-submit/
│   └── tmux-thinking.js         # marks tmux "thinking" on user input
└── lib/                         # shared utilities the hooks require()
    ├── tmux-status-coordinator.js  # CENTRAL: key/value state file at ~/.claude/hooks/data/tmux-status.json that statusline reads
    ├── status-formatter.js         # formats status strings
    ├── session-manager.js          # resolves current session's tmux window
    ├── status-history.js           # rolling history of status events
    ├── work-session.js             # tracks active work session metadata
    ├── timing-tracker.js           # records timing of tool calls
    ├── context-detector.js         # infers current context (lintMode, etc.)
    ├── process-registry.js         # tracks daemons spawned by hooks
    ├── approval-alerter-simple.js  # alerts when approval dialog shows
    ├── conversation-tracker.js     # tracks turn count / conversation id
    ├── session-cleanup.js          # reaps stale session state
    ├── tmux-window-resolver.js     # resolves current window id
    └── hook-lock.js                # prevents concurrent hook invocations
```

## Key mechanism: tmux status coordinator

The hot-path hooks all funnel through `tmux-status-coordinator.js` which maintains a JSON file (typically at `~/.claude/hooks/data/tmux-status-<session-id>.json`) with keys like:

```json
{
  "session_id": "...",
  "current_tool": "...",
  "preparing_since": 1234567890,
  "last_update": 1234567890,
  "phase": "preparing|running|ready|thinking|notifying"
}
```

The `statusline.js` reads this file and renders the tmux status-line.

**Your Go port MUST write the same JSON file with the same keys**, or the statusline goes blank. Do not reinvent the state format — read/preserve exactly what the JS version writes.

## Stdin JSON shape

All hooks receive a JSON object on stdin:

```json
{
  "session_id": "...",
  "transcript_path": "...",
  "tool_name": "Bash",                  // pre/post-tool-use only
  "tool_input": {...},                  // pre/post-tool-use only
  "tool_response": {...},               // post-tool-use only
  "requires_approval": false,           // pre-tool-use only
  "message": "...",                     // notification only
  "prompt": "..."                       // user-prompt-submit only
}
```

Unknown/extra fields should be ignored. Missing expected fields → silent exit 0.
