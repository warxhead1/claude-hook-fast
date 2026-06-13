# Emitted Signals — claude-hook-fast

This document is the authoritative reference for every field this binary emits
onto nervous-bus. It is written for:

1. **nervous-bus maintainers** who need to update `schemas/bus.agent.activity.v1.json`
   (and any other schemas listed below) to include the new optional fields.
2. **reflex-recorder / Reflexarc** downstream — exactly what to extract into
   `run.closed` features when consuming `bus.agent.activity.v1` events.

---

## Channel: `bus.agent.activity.v1`

Emitted by `cmdNbusActivity` (subcommand `nbus-activity`) on every PostToolUse
hook except Grep/Glob.

### Existing fields (unchanged)

| Field | Type | Notes |
|---|---|---|
| `ts` | RFC3339Nano string | hook-fire timestamp (UTC) |
| `event` | string | always `"tool_call"` |
| `agent` | string | always `"claude-code"` |
| `agent_kind` | string | `"host_claude_code"` or `"host_subagent"` |
| `agent_id` | string | session UUID |
| `session_id` | string | session UUID |
| `conversation_id` | string | transcript basename without `.jsonl` |
| `project` | string | canonical project name |
| `worktree` | string | worktree name or `""` for main |
| `display` | string | `"<project> [<worktree>]"` composite |
| `cwd` | string | hook CWD |
| `pane_id_qualified` | string | `tmux:<pane>` or `zellij:<sess>:<pane>` |
| `transcript_path` | string | path to JSONL transcript |
| `source_kind` | string | hook `source` field |
| `parent_tool_use_id` | string | set for subagent calls |
| `tool_name` | string | e.g. `"Bash"`, `"Read"`, `"Edit"` |
| `tool_summary` | string | first 200 chars of compacted tool_input JSON |
| `tool_response_summary` | string | first 300 chars of compacted tool_response |

### New optional fields (added in this change)

All fields are **optional** — they are omitted when unavailable. The existing
`additionalProperties: true` in `bus.agent.activity.v1.json` means consumers
already accept them. The nervous-bus schema should be updated to formally
declare them so schema validation produces better errors on bad values.

#### Token usage (from transcript tail-read)

Source: last assistant message in the transcript JSONL. Read via a cheap 8 KB
tail (see `internal/hooks/transcript/`). Only emitted when the transcript is
readable and the last assistant message has non-zero token counts.

Note: These are **per-API-call** counts, not per-tool. When Claude emits a
single tool_use per API call (the empirically confirmed common case), this is
effectively per-tool-use. On rare multi-tool-use API calls the counts are
shared across all tools in that call.

| Field | Type | Description |
|---|---|---|
| `input_tokens` | integer | `usage.input_tokens` from transcript |
| `output_tokens` | integer | `usage.output_tokens` from transcript |
| `cache_read_tokens` | integer | `usage.cache_read_input_tokens`; **omitted when 0** |
| `cache_write_tokens` | integer | `usage.cache_creation_input_tokens`; **omitted when 0** |
| `model` | string | model id, e.g. `"claude-sonnet-4-6"`, `"claude-opus-4-8"` |

#### Structured tool outcome

Source: hook stdin fields `tool_use_id` (Claude Code ~2.0+) and `tool_is_error`
(Claude Code ~2.1+). If Claude Code does not emit these fields, they are absent.

| Field | Type | Description |
|---|---|---|
| `tool_use_id` | string | stable per-invocation tool ID (`toolu_...`), matches transcript `toolUseID` |
| `tool_is_error` | boolean | true when the tool returned an error result |
| `tool_error_type` | string | short label extracted from `tool_response.type` when `tool_is_error=true`; e.g. `"exit_code"`, `"timeout"`, `"error"` (generic fallback) |

---

## For the nervous-bus side: schema update

Add the following entries to `schemas/bus.agent.activity.v1.json` under
`properties`:

```json
"input_tokens":    { "type": "integer", "minimum": 0 },
"output_tokens":   { "type": "integer", "minimum": 0 },
"cache_read_tokens":  { "type": "integer", "minimum": 0 },
"cache_write_tokens": { "type": "integer", "minimum": 0 },
"model":           { "type": "string" },
"tool_use_id":     { "type": "string" },
"tool_is_error":   { "type": "boolean" },
"tool_error_type": { "type": "string", "maxLength": 64 }
```

These fields are **not** required (do not add them to `required`). They are
additive and backward-compatible.

---

## For reflex-recorder: `run.closed` features

When consuming `bus.agent.activity.v1` to build `run.closed` features, fold
in the following additional extractors:

### Per-turn token efficiency

```python
# Fold into run.closed.features during run aggregation:
features["tokens_input_total"]  += event.get("input_tokens", 0)
features["tokens_output_total"] += event.get("output_tokens", 0)
features["tokens_cache_read"]   += event.get("cache_read_tokens", 0)
features["tokens_cache_write"]  += event.get("cache_write_tokens", 0)
# Cache hit rate (fraction of reads vs writes — proxy for prompt reuse):
if features["tokens_cache_write"] > 0:
    features["cache_hit_rate"] = features["tokens_cache_read"] / features["tokens_cache_write"]
```

### Model tracking

```python
# Track which model was used most in this run (for multi-model sessions):
models_seen = features.get("models_seen", {})
if model := event.get("model"):
    models_seen[model] = models_seen.get(model, 0) + 1
features["models_seen"] = models_seen
features["primary_model"] = max(models_seen, key=models_seen.get) if models_seen else None
```

### Tool error rate

```python
# Run-level tool error rate for reliability signals:
if event.get("tool_is_error"):
    features["tool_errors"] = features.get("tool_errors", 0) + 1
    err_type = event.get("tool_error_type", "error")
    err_key = f"tool_error_{err_type}"
    features[err_key] = features.get(err_key, 0) + 1
# Compute rate at run.closed time:
features["tool_error_rate"] = features.get("tool_errors", 0) / max(features.get("tool_calls", 1), 1)
```

### tool_use_id for deduplication

If reflex-recorder processes activity events more than once (e.g. re-ingestion
from Redis replay), use `tool_use_id` as a deduplication key:

```python
seen_tool_use_ids = set()
if (tid := event.get("tool_use_id")) and tid in seen_tool_use_ids:
    continue  # skip duplicate
seen_tool_use_ids.add(tid)
```

---

## Cheapness

| Signal | Source | Cost |
|---|---|---|
| `tool_use_id`, `tool_is_error` | hook stdin (already read) | 0 extra syscalls |
| `tool_error_type` | `tool_response` JSON (already parsed) | 1 extra `json.Unmarshal` |
| token fields + model | last 8 KB of transcript JSONL | 1 `open`, 1 `seek`, 1 `read` |

The transcript tail-read adds one file open+seek+read per PostToolUse event
that has a non-empty `transcript_path`. Benchmarks on a warm filesystem:
< 0.5 ms on typical NVMe hardware. The binary remains < 7 MB with standard
Go toolchain build flags (`-trimpath -ldflags='-s -w'`).

Binary size: **6.58 MB** (was **6.58 MB** before, +12 KB for transcript package).
