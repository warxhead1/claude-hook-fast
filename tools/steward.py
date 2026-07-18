#!/usr/bin/env python3
"""claude-steward — Python reference implementation (prototype tier).

# Interchangeability contract

This mirrors the Go policy core at internal/steward/{types,decide,actuate,
ports}.go. The point: prototype policy tweaks here, watch actions.jsonl
against reality, then port stable changes back into decide.go. That only
works if both sides honor, identically:

  1. The SAME input file paths (usage cache, session registry, statusline
     context files, daemon roster, subagent transcripts) — see the `read_*`
     functions below and docs/design-budget-steward.md's signal inventory.
  2. The SAME ~/.claude/steward/budget.json schema/keys as Go's BudgetDoc.
  3. The SAME send-keys safety protocol (idle-only, pane-command
     verification, audit-before-send, no retry loop) for tmux, and the same
     framed-socket handshake for the ptysock fallback.
  4. The SAME pause (~/.claude/steward/pause), per-session ignore
     (~/.claude/steward/ignore/<id>), and single-instance lock
     (~/.claude/steward/lock) files.

stdlib-only by design (json, os, socket, struct, time, glob, argparse,
subprocess, dataclasses, enum) — no venv, no deps, runs anywhere Python 3
runs. Promote stable policy to Go; do not let the two forks drift.

Subagent completion is read exactly as Go's reader reads it (see completion.go):
NOT from the SubagentStart/SubagentStop hooks — those cannot identify an
individual subagent (subagent_id is never populated; SubagentStop carries only
the parent's identity). A dispatched subagent's .meta.json carries a `toolUseId`,
and its completion is recorded in the PARENT session transcript as a
`tool_result` bearing that id. Each parent transcript is tailed incrementally
(append-only JSONL; cursor offset + completed-id set persisted in state.json's
`completions` map) and a subagent is marked `stopped` once its `tool_result`
appears. Subagents with no `toolUseId` (in-process teammates) fall back to
mtime staleness alone. The Lingering() predicate is byte-identical to Go's.
"""

from __future__ import annotations

import argparse
import fcntl
import glob
import json
import os
import socket
import struct
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from enum import IntEnum
from typing import Optional

# ===== 1. Pure policy core — mirrors internal/steward/{types,decide,actuate}.go =====


class Level(IntEnum):
    NORMAL = 0
    WATCH = 1
    PRESSURE = 2
    CRITICAL = 3

    def __str__(self) -> str:
        return ("normal", "watch", "pressure", "critical")[self]


class SessionKind(IntEnum):
    INTERACTIVE = 0
    BACKGROUND = 1

    def __str__(self) -> str:
        return "background" if self is SessionKind.BACKGROUND else "interactive"


class Method(IntEnum):
    NONE = 0
    TMUX = 1
    PTYSOCK = 2

    def __str__(self) -> str:
        return {Method.TMUX: "tmux", Method.PTYSOCK: "ptysock"}.get(self, "none")


class ActionKind(IntEnum):
    SET_BUDGET, NOTIFY, COMPACT, REPORT_SUBAGENT, SET_FANOUT_GATE = range(5)


FANOUT_ALLOW, FANOUT_WARN, FANOUT_BLOCK = "allow", "warn", "block"


@dataclass
class Window:
    utilization: float = 0.0
    resets_at: Optional[datetime] = None
    present: bool = False


@dataclass
class SessionState:
    id: str
    name: str = ""
    kind: SessionKind = SessionKind.INTERACTIVE
    status: str = ""  # "busy" | "idle" | "waiting"
    context_pct: int = 0
    idle_for: timedelta = timedelta(0)
    cache_age: timedelta = timedelta(0)
    subagents_active: int = 0
    pane_id: str = ""
    pty_sock: str = ""
    pty_auth_avail: bool = False
    last_compact: Optional[datetime] = None
    ignored: bool = False


@dataclass
class SubagentState:
    id: str
    parent_id: str
    name: str = ""
    started: bool = True
    stopped: bool = False
    stale_for: timedelta = timedelta(0)

    def lingering(self, threshold: timedelta) -> bool:
        return self.started and not self.stopped and self.stale_for > threshold


@dataclass
class Config:
    enable_notify: bool = True
    enable_rules_gate: bool = False
    enable_compact: bool = False
    enable_fanout_block: bool = False
    watch_pct: float = 60.0
    pressure_pct: float = 75.0
    critical_pct: float = 90.0
    hysteresis: float = 5.0
    min_context_pct: int = 55
    compact_cooldown: timedelta = timedelta(minutes=30)
    min_reset_headroom: timedelta = timedelta(minutes=20)
    min_idle_for_compact: timedelta = timedelta(minutes=2)
    cache_expiry_horizon: timedelta = timedelta(minutes=55)
    lingering_subagent_after: timedelta = timedelta(minutes=10)
    paused: bool = False
    max_compacts_per_tick: int = 3


def default_config() -> Config:
    """Phase-0 (observer-only) defaults — identical to Go's DefaultConfig()."""
    return Config()


@dataclass
class Snapshot:
    now: datetime
    five_hour: Window = field(default_factory=Window)
    seven_day: Window = field(default_factory=Window)
    burn_pct_per_min: float = 0.0
    sessions: list = field(default_factory=list)
    subagents: list = field(default_factory=list)
    prev_level: Level = Level.NORMAL
    config: Config = field(default_factory=default_config)

    def time_to_reset(self) -> timedelta:
        if self.five_hour.resets_at is None:
            return timedelta(0)
        d = self.five_hour.resets_at - self.now
        return d if d > timedelta(0) else timedelta(0)

    def forecast_exhaust(self) -> timedelta:
        if self.burn_pct_per_min <= 0:
            return timedelta(0)
        remaining = 100 - self.five_hour.utilization
        if remaining <= 0:
            return timedelta(0)
        return timedelta(minutes=remaining / self.burn_pct_per_min)


@dataclass
class Action:
    kind: ActionKind
    level: Level
    session_id: str = ""
    method: Method = Method.NONE
    gate: str = ""
    reason: str = ""


def resolve_actuation(s: SessionState) -> Method:
    """actuate.go ResolveActuation: tmux first (broader, safer failure mode);
    ptysock only for a paneless daemon worker with a readable auth secret."""
    if s.pane_id:
        return Method.TMUX
    if s.pty_sock and s.pty_auth_avail:
        return Method.PTYSOCK
    return Method.NONE


def _entry_threshold(level: Level, cfg: Config) -> float:
    return {Level.WATCH: cfg.watch_pct, Level.PRESSURE: cfg.pressure_pct,
            Level.CRITICAL: cfg.critical_pct}.get(level, 0.0)


def compute_level(snap: Snapshot) -> Level:
    """decide.go computeLevel: threshold ladder, forecast escalation, downward
    hysteresis only — upward transitions are never damped."""
    cfg = snap.config
    u = snap.five_hour.utilization
    level = Level.NORMAL
    if u >= cfg.watch_pct:
        level = Level.WATCH
    if u >= cfg.pressure_pct:
        level = Level.PRESSURE
    if u >= cfg.critical_pct:
        level = Level.CRITICAL

    fe = snap.forecast_exhaust()
    if fe > timedelta(0):
        ttr = snap.time_to_reset()
        if ttr > timedelta(0) and fe < ttr and level < Level.PRESSURE:
            level = Level.PRESSURE

    if level < snap.prev_level and u > _entry_threshold(snap.prev_level, cfg) - cfg.hysteresis:
        level = snap.prev_level
    return level


def fanout_gate(level: Level, cfg: Config) -> str:
    """decide.go fanoutGate."""
    if level is Level.PRESSURE:
        return FANOUT_WARN
    if level is Level.CRITICAL:
        return FANOUT_BLOCK if (cfg.enable_fanout_block and not cfg.paused) else FANOUT_WARN
    return FANOUT_ALLOW


def compact_eligible(s: SessionState, snap: Snapshot) -> bool:
    """decide.go compactEligible, clause for clause."""
    cfg = snap.config
    if s.ignored or s.status != "idle":
        return False
    if s.idle_for < cfg.min_idle_for_compact or s.context_pct < cfg.min_context_pct:
        return False
    if s.subagents_active > 0:
        return False
    if s.last_compact is not None and (snap.now - s.last_compact) < cfg.compact_cooldown:
        return False
    return resolve_actuation(s) != Method.NONE


def rank_eligible(snap: Snapshot) -> list:
    """decide.go rankEligible: (cache-age desc, context desc, idle desc),
    capped at MaxCompactsPerTick."""
    elig = [s for s in snap.sessions if compact_eligible(s, snap)]
    elig.sort(key=lambda s: (s.cache_age, s.context_pct, s.idle_for), reverse=True)
    cap = snap.config.max_compacts_per_tick
    return elig[:cap] if cap > 0 and len(elig) > cap else elig


def compact_reason(s: SessionState, level: Level) -> str:
    return f"compact idle session at {level} (ctx={s.context_pct}%, via {resolve_actuation(s)})"


def decide(snap: Snapshot) -> list:
    """decide.go Decide — identical action order and gating."""
    cfg = snap.config
    level = compute_level(snap)
    actions = [Action(ActionKind.SET_BUDGET, level, reason="budget snapshot")]

    if cfg.enable_rules_gate:
        gate = fanout_gate(level, cfg)
        actions.append(Action(ActionKind.SET_FANOUT_GATE, level, gate=gate,
                               reason=f"fan-out gate for level {level}"))

    if cfg.enable_notify and level >= Level.WATCH and level > snap.prev_level:
        actions.append(Action(ActionKind.NOTIFY, level, reason=f"budget level rose to {level}"))

    if cfg.enable_notify:
        for sub in snap.subagents:
            if sub.lingering(cfg.lingering_subagent_after):
                actions.append(Action(ActionKind.REPORT_SUBAGENT, level, session_id=sub.parent_id,
                                       reason=f"lingering subagent {sub.name}"))

    if cfg.enable_compact and not cfg.paused and level >= Level.PRESSURE:
        ttr = snap.time_to_reset()
        if ttr == timedelta(0) or ttr >= cfg.min_reset_headroom:
            for s in rank_eligible(snap):
                actions.append(Action(ActionKind.COMPACT, level, session_id=s.id,
                                       method=resolve_actuation(s), reason=compact_reason(s, level)))
    return actions


# ===== 2. Readers — same input files as the Go readers (design doc signal table) =====

USAGE_CACHE_PATH = "/tmp/claude-usage-cache.json"
SESSIONS_DIR = os.path.expanduser("~/.claude/sessions")
ROSTER_PATH = os.path.expanduser("~/.claude/daemon/roster.json")
PROJECTS_DIR = os.path.expanduser("~/.claude/projects")


def _context_dir() -> str:
    return os.path.join(os.environ.get("XDG_RUNTIME_DIR") or "/run/user/1000", "hearth", "context")


def _parse_dt(s: Optional[str]) -> Optional[datetime]:
    if not s:
        return None
    try:
        dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
        return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)
    except ValueError:
        return None


def _parse_window(obj: Optional[dict]) -> Window:
    if not isinstance(obj, dict):
        return Window(present=False)
    u = obj.get("utilization", obj.get("used_percentage"))
    if u is None:
        return Window(present=False)
    return Window(utilization=float(u), resets_at=_parse_dt(obj.get("resets_at")), present=True)


def read_usage_windows(path: str = USAGE_CACHE_PATH) -> tuple:
    """/tmp/claude-usage-cache.json. Tolerates utilization|used_percentage and
    an optional top-level 'usage' wrapper — the endpoint is community-
    reverse-engineered and its shape has drifted across CLI versions."""
    try:
        with open(path, "r") as f:
            raw = json.load(f)
    except (OSError, json.JSONDecodeError):
        return Window(present=False), Window(present=False)
    data = raw.get("usage", raw) if isinstance(raw, dict) else {}
    return _parse_window(data.get("five_hour")), _parse_window(data.get("seven_day"))


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except (OSError, ValueError):
        return False
    return True


def read_sessions(now: datetime) -> list:
    """~/.claude/sessions/<pid>.json, skipping dead pids."""
    out = []
    for path in glob.glob(os.path.join(SESSIONS_DIR, "*.json")):
        try:
            with open(path, "r") as f:
                doc = json.load(f)
        except (OSError, json.JSONDecodeError):
            continue
        pid, sid = doc.get("pid"), doc.get("sessionId") or ""
        if not isinstance(pid, int) or not _pid_alive(pid) or not sid:
            continue
        ua = doc.get("updatedAt")
        updated = (datetime.fromtimestamp(ua / 1000.0, tz=timezone.utc)
                   if isinstance(ua, (int, float)) else (_parse_dt(ua) or now))
        kind = SessionKind.BACKGROUND if doc.get("kind") == "background" else SessionKind.INTERACTIVE
        out.append(SessionState(id=sid, name=doc.get("name", ""), kind=kind,
                                 status=doc.get("status", ""), idle_for=max(now - updated, timedelta(0))))
    return out


def apply_context_files(sessions: list, now: datetime) -> None:
    """Overlays currentContext%% and pane binding from statusline context
    files. Subagent entries (is_subagent) never bind a pane."""
    by_sid = {s.id: s for s in sessions}
    for path in glob.glob(os.path.join(_context_dir(), "claude-context-*.json")):
        sid = os.path.basename(path)[len("claude-context-"):-len(".json")]
        s = by_sid.get(sid)
        if s is None:
            continue
        try:
            with open(path, "r") as f:
                doc = json.load(f)
        except (OSError, json.JSONDecodeError):
            continue
        s.context_pct = int(doc.get("currentContext") or 0)
        if doc.get("is_subagent"):
            continue
        if doc.get("tmux_pane"):
            s.pane_id = f"tmux:{doc['tmux_pane']}"
        mtime = os.path.getmtime(path)
        s.cache_age = max(now - datetime.fromtimestamp(mtime, tz=timezone.utc), timedelta(0))


def apply_roster(sessions: list) -> None:
    """Overlays ptySock + a ptyAuth-PRESENCE boolean only — NEVER copy the
    secret itself into the snapshot."""
    try:
        with open(ROSTER_PATH, "r") as f:
            workers = json.load(f).get("workers", {})
    except (OSError, json.JSONDecodeError):
        return
    by_sid = {s.id: s for s in sessions}
    for w in workers.values():
        s = by_sid.get(w.get("sessionId"))
        if s is None:
            continue
        s.pty_sock = w.get("ptySock", "") or ""
        s.pty_auth_avail = bool(w.get("ptyAuth"))
        if s.pty_sock and not s.pane_id:
            s.kind = SessionKind.BACKGROUND


def _scan_completed_ids(line: str, dst: set) -> None:
    """Fold the tool_use_id of every tool_result carried by one transcript line
    into dst. The substring gate skips the majority of lines that carry no
    tool_result before the JSON parse; a line whose message.content is not a
    list is skipped (never carries a tool_result)."""
    if '"tool_result"' not in line:
        return
    try:
        rec = json.loads(line)
    except (json.JSONDecodeError, ValueError):
        return
    msg = rec.get("message")
    content = msg.get("content") if isinstance(msg, dict) else None
    if not isinstance(content, list):
        return
    for item in content:
        if isinstance(item, dict) and item.get("type") == "tool_result" and item.get("tool_use_id"):
            dst.add(item["tool_use_id"])


def _completed_tool_ids(state: dict, path: str) -> set:
    """Incrementally tail the parent transcript at `path`, advancing the cursor
    stored in state['completions'][path] over only newly-appended whole lines,
    and return the set of tool_use_ids seen completed. Mirrors Go's
    completionTracker.advance: append-only JSONL, partial final line re-read
    next tick, a shorter-than-offset file (rotation) resets the cursor."""
    cursors = state.setdefault("completions", {})
    cur = cursors.setdefault(path, {"offset": 0, "ids": []})
    ids = set(cur["ids"])
    try:
        size = os.path.getsize(path)
    except OSError:
        return ids
    offset = cur["offset"]
    if size < offset:            # truncated / rotated
        offset, ids = 0, set()
    try:
        with open(path, "rb") as f:
            f.seek(offset)
            data = f.read()
    except OSError:
        return ids
    last_nl = data.rfind(b"\n")  # consume only through the last whole line
    if last_nl >= 0:
        whole = data[:last_nl + 1]
        offset += len(whole)
        for raw in whole.split(b"\n"):
            if raw:
                _scan_completed_ids(raw.decode("utf-8", "replace"), ids)
    cur["offset"], cur["ids"] = offset, sorted(ids)
    return ids


def _prune_completions(state: dict) -> None:
    """Drop completion cursors whose transcript no longer exists (ended
    sessions), keeping state.json bounded by live churn."""
    cursors = state.get("completions")
    if not cursors:
        return
    for path in [p for p in cursors if not os.path.exists(p)]:
        del cursors[path]


def read_subagents(sessions: list, now: datetime, cfg: Config, state: dict) -> list:
    """Globs ~/.claude/projects/*/<sid>/subagents/agent-*.jsonl per session;
    mtime age -> StaleFor. `stopped` is resolved from the parent transcript's
    tool_result for the subagent's meta `toolUseId` (see module docstring); a
    subagent with no toolUseId or an unconfirmed completion stays stopped=False
    so mtime staleness alone decides. A completed subagent is never active."""
    out = []
    active_cutoff = cfg.lingering_subagent_after
    for s in sessions:
        pattern = os.path.join(PROJECTS_DIR, "*", s.id, "subagents", "agent-*.jsonl")
        active = 0
        for path in glob.glob(pattern):
            try:
                mtime = os.path.getmtime(path)
            except OSError:
                continue
            stale_for = max(now - datetime.fromtimestamp(mtime, tz=timezone.utc), timedelta(0))
            stem = os.path.basename(path)[:-len(".jsonl")]
            name, tool_use_id = stem, ""
            try:
                with open(path[:-len(".jsonl")] + ".meta.json", "r") as f:
                    meta = json.load(f)
                name = meta.get("name", name)
                tool_use_id = meta.get("toolUseId", "")
            except (OSError, json.JSONDecodeError):
                pass
            stopped = False
            if tool_use_id:
                # subagents/ sits directly under the parent session dir; the
                # parent transcript is that dir + ".jsonl".
                parent_transcript = os.path.dirname(os.path.dirname(path)) + ".jsonl"
                stopped = tool_use_id in _completed_tool_ids(state, parent_transcript)
            out.append(SubagentState(id=stem, parent_id=s.id, name=name,
                                     stopped=stopped, stale_for=stale_for))
            active += (not stopped) and stale_for < active_cutoff
        s.subagents_active = active
    return out


def read_ignored(sessions: list, config_root: str) -> None:
    ignore_dir = os.path.join(config_root, "ignore")
    for s in sessions:
        s.ignored = os.path.exists(os.path.join(ignore_dir, s.id))


def read_pause(config_root: str) -> bool:
    return os.path.exists(os.path.join(config_root, "pause"))


# ---- persisted cross-tick state: prev level, per-session cooldown, burn EMA ----

def load_state(config_root: str) -> dict:
    try:
        with open(os.path.join(config_root, "state.json"), "r") as f:
            return json.load(f)
    except (OSError, json.JSONDecodeError):
        return {"prev_level": 0, "last_compact": {}, "burn": {}}


def save_state(config_root: str, state: dict) -> None:
    os.makedirs(config_root, exist_ok=True)
    _atomic_write_json(os.path.join(config_root, "state.json"), state)


def apply_last_compact(sessions: list, state: dict) -> None:
    lc = state.get("last_compact", {})
    for s in sessions:
        if lc.get(s.id):
            s.last_compact = _parse_dt(lc[s.id])


def compute_burn_rate(now: datetime, five_hour: Window, state: dict) -> float:
    """EMA of five-hour utilization %/min, persisted in state.json (the
    design doc's 'cursors' persistence). 0 if unknown."""
    burn = state.get("burn", {})
    last_ts, last_util, ema = _parse_dt(burn.get("last_ts")), burn.get("last_util"), burn.get("ema", 0.0)
    if last_ts is not None and last_util is not None:
        dt_min = (now - last_ts).total_seconds() / 60.0
        if dt_min > 0:
            inst = max((five_hour.utilization - last_util) / dt_min, 0.0)
            ema = 0.3 * inst + 0.7 * ema
    state["burn"] = {"last_ts": now.isoformat(), "last_util": five_hour.utilization, "ema": ema}
    return ema


def build_snapshot(now: datetime, cfg: Config, config_root: str, prev_level: Level, state: dict) -> Snapshot:
    """Reads all live inputs and overlays the persisted `state` dict (cooldown
    map + burn EMA) onto them. Mutates `state` in place (burn EMA refresh);
    the caller owns load/save so there is exactly one read-modify-write of
    state.json per tick — see run_tick."""
    five_hour, seven_day = read_usage_windows()
    sessions = read_sessions(now)
    apply_context_files(sessions, now)
    apply_roster(sessions)
    subagents = read_subagents(sessions, now, cfg, state)
    _prune_completions(state)
    read_ignored(sessions, config_root)

    apply_last_compact(sessions, state)
    burn = compute_burn_rate(now, five_hour, state)

    return Snapshot(now=now, five_hour=five_hour, seven_day=seven_day, burn_pct_per_min=burn,
                     sessions=sessions, subagents=subagents, prev_level=prev_level, config=cfg)


# ===== 3. Budget doc + executor (dry-run-first) =====

def _window_doc(w: Window) -> dict:
    d = {"utilization": w.utilization, "present": w.present}
    if w.resets_at is not None:
        d["resets_at"] = w.resets_at.astimezone(timezone.utc).isoformat()
    return d


def build_budget_doc(snap: Snapshot, level: Level, gate: str, now_rfc3339: str) -> dict:
    """ports.go BuildBudgetDoc, field for field, including omitempty
    semantics: forecast_exhaust_at / lingering_subagents are omitted (not
    null/empty) exactly as Go's encoding/json omitempty does."""
    doc = {"state": str(level), "updated_at": now_rfc3339, "five_hour": _window_doc(snap.five_hour),
           "seven_day": _window_doc(snap.seven_day), "burn_rate_pct_per_min": snap.burn_pct_per_min,
           "fanout_gate": gate, "sessions": []}
    fe = snap.forecast_exhaust()
    if fe > timedelta(0):
        doc["forecast_exhaust_at"] = (snap.now + fe).astimezone(timezone.utc).isoformat()
    for s in snap.sessions:
        doc["sessions"].append({
            "id": s.id, "name": s.name, "kind": str(s.kind), "status": s.status,
            "context_pct": s.context_pct, "idle_for_s": int(s.idle_for.total_seconds()),
            "cache_age_s": int(s.cache_age.total_seconds()), "subagents_active": s.subagents_active,
            "actuation_method": str(resolve_actuation(s)), "compact_eligible": compact_eligible(s, snap),
        })
    lingering = [sub for sub in snap.subagents if sub.lingering(snap.config.lingering_subagent_after)]
    if lingering:
        doc["lingering_subagents"] = [
            {"id": sub.id, "parent_id": sub.parent_id, "name": sub.name,
             "stale_for_s": int(sub.stale_for.total_seconds())} for sub in lingering]
    return doc


def _atomic_write_json(path: str, doc: dict) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(doc, f, indent=2, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)


def _append_jsonl(path: str, record: dict) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "a") as f:
        f.write(json.dumps(record, sort_keys=True) + "\n")


def _tmux_pane_running_claude(pane: str) -> bool:
    try:
        out = subprocess.run(["tmux", "display", "-p", "-t", pane, "#{pane_current_command}"],
                              capture_output=True, text=True, timeout=3)
    except (OSError, subprocess.SubprocessError):
        return False
    return out.returncode == 0 and "claude" in out.stdout.lower()


def compact_via_tmux(pane_id: str) -> bool:
    """Same safety gate as Go: verify the pane is actually running claude
    before ever sending keys, then literal /compact + a separate Enter."""
    pane = pane_id.split(":", 1)[1] if ":" in pane_id else pane_id
    if not _tmux_pane_running_claude(pane):
        return False
    try:
        subprocess.run(["tmux", "send-keys", "-t", pane, "-l", "/compact"], check=True, timeout=3)
        subprocess.run(["tmux", "send-keys", "-t", pane, "Enter"], check=True, timeout=3)
    except (OSError, subprocess.SubprocessError):
        return False
    return True


def _sock_send(sock: socket.socket, kind: int, payload: bytes) -> None:
    sock.sendall(struct.pack(">I", len(payload)) + bytes([kind]) + payload)


def _sock_recv_frame(sock: socket.socket):
    header = sock.recv(5)
    if len(header) < 5:
        return None, None
    length, kind, buf = struct.unpack(">I", header[:4])[0], header[4], b""
    while len(buf) < length:
        chunk = sock.recv(length - len(buf))
        if not chunk:
            break
        buf += chunk
    return kind, buf


def compact_via_ptysock(sock_path: str, timeout: float = 3.0) -> bool:
    """Experimental framed-socket injection for headless --bg/--fork workers
    with no tmux pane (see design doc's bg-pty-host wire protocol). Same
    --enable-compact gate, default OFF. Reads ptyAuth from the roster at call
    time only — never persisted on SessionState/Snapshot."""
    try:
        with open(ROSTER_PATH, "r") as f:
            workers = json.load(f).get("workers", {})
    except (OSError, json.JSONDecodeError):
        return False
    token = next((w.get("ptyAuth") for w in workers.values() if w.get("ptySock") == sock_path), None)
    if not token:
        return False
    try:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(timeout)
        sock.connect(sock_path)
        _sock_send(sock, 1, json.dumps({"t": "pong"}).encode())
        _sock_send(sock, 1, json.dumps({"t": "auth", "token": token}).encode())
        deadline = time.time() + timeout
        while time.time() < deadline:
            kind, buf = _sock_recv_frame(sock)
            if kind is None:
                break
            if kind == 1:
                try:
                    msg = json.loads(buf)
                except json.JSONDecodeError:
                    continue
                if msg.get("t") == "ping":
                    _sock_send(sock, 1, json.dumps({"t": "pong"}).encode())
                if msg.get("t") == "live":
                    break
        _sock_send(sock, 0, b"/compact\r")
        sock.close()
        return True
    except (OSError, socket.error):
        return False


def run_tick(cfg: Config, config_root: str, dry_run: bool) -> Level:
    now = datetime.now(timezone.utc)
    state = load_state(config_root)
    prev_level = Level(state.get("prev_level", 0))
    cfg.paused = read_pause(config_root)

    snap = build_snapshot(now, cfg, config_root, prev_level, state)
    level = compute_level(snap)
    gate = fanout_gate(level, cfg)
    actions = decide(snap)

    now_rfc3339 = now.isoformat()
    last_compact = state.get("last_compact", {})

    for act in actions:
        if act.kind == ActionKind.SET_BUDGET:
            _atomic_write_json(os.path.join(config_root, "budget.json"),
                                build_budget_doc(snap, level, gate, now_rfc3339))
        elif act.kind == ActionKind.SET_FANOUT_GATE:
            _atomic_write_json(os.path.join(config_root, "gate.json"),
                                {"fanout_gate": act.gate, "level": str(act.level), "updated_at": now_rfc3339})
        elif act.kind == ActionKind.NOTIFY:
            print(f"[steward] NOTIFY: {act.reason}")
        elif act.kind == ActionKind.REPORT_SUBAGENT:
            print(f"[steward] LINGERING SUBAGENT parent={act.session_id}: {act.reason}")
        elif act.kind == ActionKind.COMPACT:
            sess = next((s for s in snap.sessions if s.id == act.session_id), None)
            actuated = False
            if dry_run:
                print(f"[steward] [DRY RUN] would compact {act.session_id} via {act.method}: {act.reason}")
            else:
                print(f"[steward] compacting {act.session_id} via {act.method}: {act.reason}")
                if sess is not None and act.method == Method.TMUX:
                    actuated = compact_via_tmux(sess.pane_id)
                elif sess is not None and act.method == Method.PTYSOCK:
                    actuated = compact_via_ptysock(sess.pty_sock)
                if not actuated:
                    print(f"[steward] WARN: actuation failed/skipped for {act.session_id}; "
                          "do-not-touch this tick, no retry")
            _append_jsonl(os.path.join(config_root, "actions.jsonl"), {
                "ts": now_rfc3339, "action": "compact", "session_id": act.session_id,
                "method": str(act.method), "reason": act.reason, "dry_run": dry_run, "actuated": actuated,
            })
            # Cooldown applies to dry runs too — repeating the same "would
            # compact" target every tick is noise, not signal.
            last_compact[act.session_id] = now_rfc3339

    state["prev_level"], state["last_compact"] = int(level), last_compact
    save_state(config_root, state)
    return level


# ===== 4. Self-test — plain-assert decision table, mirrors decide_test.go cases =====

def _mk_session(**kwargs) -> SessionState:
    base = dict(id="s1", status="idle", context_pct=70, idle_for=timedelta(minutes=10),
                cache_age=timedelta(minutes=50), pane_id="tmux:%1")
    base.update(kwargs)
    return SessionState(**base)


def _snap(**kwargs) -> Snapshot:
    kwargs.setdefault("now", datetime(2026, 7, 18, 12, 0, 0, tzinfo=timezone.utc))
    kwargs.setdefault("config", default_config())
    return Snapshot(**kwargs)


def selftest() -> None:
    now = datetime(2026, 7, 18, 12, 0, 0, tzinfo=timezone.utc)

    # level entry thresholds
    assert compute_level(_snap(five_hour=Window(50, present=True))) == Level.NORMAL
    assert compute_level(_snap(five_hour=Window(65, present=True))) == Level.WATCH
    assert compute_level(_snap(five_hour=Window(80, present=True))) == Level.PRESSURE
    assert compute_level(_snap(five_hour=Window(95, present=True))) == Level.CRITICAL

    # hysteresis: hold at 72 (>70=75-5), release at 65 (<70)
    assert compute_level(_snap(five_hour=Window(72, present=True), prev_level=Level.PRESSURE)) == Level.PRESSURE
    assert compute_level(_snap(five_hour=Window(65, present=True), prev_level=Level.PRESSURE)) == Level.WATCH

    # forecast escalation: instantaneous NORMAL, but exhaust-before-reset forces PRESSURE
    snap = _snap(five_hour=Window(50, resets_at=now + timedelta(minutes=60), present=True), burn_pct_per_min=2.0)
    assert compute_level(snap) == Level.PRESSURE  # (100-50)/2 = 25min < 60min reset

    # eligibility: baseline eligible, then one exclusion per clause
    ok = _mk_session()
    assert compact_eligible(ok, _snap(five_hour=Window(80, present=True), sessions=[ok])) is True
    exclusions = [
        _mk_session(ignored=True), _mk_session(status="busy"), _mk_session(status="waiting"),
        _mk_session(idle_for=timedelta(seconds=30)), _mk_session(context_pct=40),
        _mk_session(subagents_active=1), _mk_session(last_compact=now - timedelta(minutes=5)),
        _mk_session(pane_id="", pty_sock="", pty_auth_avail=False),
    ]
    for s in exclusions:
        assert compact_eligible(s, _snap(five_hour=Window(80, present=True), sessions=[s])) is False, s
    elapsed = _mk_session(last_compact=now - timedelta(minutes=31))
    assert compact_eligible(elapsed, _snap(five_hour=Window(80, present=True), sessions=[elapsed])) is True

    # ranking + cap: cache-age desc, then context desc, then idle desc
    sessions = [
        _mk_session(id="a", cache_age=timedelta(minutes=10), context_pct=60, idle_for=timedelta(minutes=5)),
        _mk_session(id="b", cache_age=timedelta(minutes=50), context_pct=60, idle_for=timedelta(minutes=5)),
        _mk_session(id="c", cache_age=timedelta(minutes=50), context_pct=90, idle_for=timedelta(minutes=5)),
        _mk_session(id="d", cache_age=timedelta(minutes=5), context_pct=99, idle_for=timedelta(minutes=99)),
    ]
    cfg_cap2 = default_config()
    cfg_cap2.max_compacts_per_tick = 2
    ranked = rank_eligible(_snap(five_hour=Window(80, present=True), config=cfg_cap2, sessions=sessions))
    assert [s.id for s in ranked] == ["c", "b"], [s.id for s in ranked]

    # pause halts compaction and downgrades block -> warn; unpaused CRITICAL blocks + compacts
    elig = _mk_session()
    cfg_paused = default_config()
    cfg_paused.enable_compact = cfg_paused.enable_rules_gate = cfg_paused.enable_fanout_block = True
    cfg_paused.paused = True
    actions = decide(_snap(five_hour=Window(95, present=True), config=cfg_paused, sessions=[elig]))
    assert not any(a.kind == ActionKind.COMPACT for a in actions)
    assert next(a for a in actions if a.kind == ActionKind.SET_FANOUT_GATE).gate == FANOUT_WARN

    cfg_live = default_config()
    cfg_live.enable_compact = cfg_live.enable_rules_gate = cfg_live.enable_fanout_block = True
    actions = decide(_snap(five_hour=Window(95, present=True), config=cfg_live, sessions=[elig]))
    assert next(a for a in actions if a.kind == ActionKind.SET_FANOUT_GATE).gate == FANOUT_BLOCK
    assert any(a.kind == ActionKind.COMPACT and a.session_id == "s1" for a in actions)

    # notify fires once, only on an upward transition
    actions = decide(_snap(five_hour=Window(65, present=True), prev_level=Level.NORMAL))
    assert any(a.kind == ActionKind.NOTIFY for a in actions)
    actions = decide(_snap(five_hour=Window(65, present=True), prev_level=Level.WATCH))
    assert not any(a.kind == ActionKind.NOTIFY for a in actions)

    # lingering subagent reported + its parent excluded from compaction
    parent = _mk_session(id="p1", subagents_active=1)
    sub = SubagentState(id="sub1", parent_id="p1", name="worker", stale_for=timedelta(minutes=15))
    cfg_c = default_config()
    cfg_c.enable_compact = True
    actions = decide(_snap(five_hour=Window(80, present=True), config=cfg_c, sessions=[parent], subagents=[sub]))
    assert any(a.kind == ActionKind.REPORT_SUBAGENT and a.session_id == "p1" for a in actions)
    assert not any(a.kind == ActionKind.COMPACT and a.session_id == "p1" for a in actions)

    # ResolveActuation precedence + MinResetHeadroom suppression
    assert resolve_actuation(SessionState(id="x", pane_id="tmux:%9", pty_sock="/tmp/x.sock",
                                           pty_auth_avail=True)) == Method.TMUX
    assert resolve_actuation(SessionState(id="y", pty_sock="/tmp/y.sock", pty_auth_avail=True)) == Method.PTYSOCK
    assert resolve_actuation(SessionState(id="z", pty_sock="/tmp/z.sock", pty_auth_avail=False)) == Method.NONE

    cfg_r = default_config()
    cfg_r.enable_compact = True
    snap = _snap(five_hour=Window(80, resets_at=now + timedelta(minutes=5), present=True),
                 config=cfg_r, sessions=[_mk_session()])
    assert not any(a.kind == ActionKind.COMPACT for a in decide(snap))

    # subagent completion detection: incremental cursor + stopped resolution
    import tempfile
    def _tr(tid):
        return ('{"type":"user","message":{"role":"user","content":'
                '[{"type":"tool_result","tool_use_id":"%s","content":"ok"}]}}\n' % tid)
    with tempfile.TemporaryDirectory() as td:
        tpath = os.path.join(td, "parent.jsonl")
        with open(tpath, "w") as f:
            f.write(_tr("toolu_A"))
        st = {}
        assert "toolu_A" in _completed_tool_ids(st, tpath)
        assert "toolu_B" not in _completed_tool_ids(st, tpath)
        # a tool_use dispatch is not a completion; append + incremental pickup
        with open(tpath, "a") as f:
            f.write('{"type":"assistant","message":{"role":"assistant","content":'
                    '[{"type":"tool_use","id":"toolu_X","name":"Task"}]}}\n')
            f.write(_tr("toolu_B"))
        got = _completed_tool_ids(st, tpath)
        assert "toolu_B" in got and "toolu_X" not in got, got
        # a stale, completed subagent is stopped and not lingering
        subdir = os.path.join(td, "proj", "sess1", "subagents")
        os.makedirs(subdir)
        agent = os.path.join(subdir, "agent-done.jsonl")
        with open(agent, "w") as f:
            f.write("{}\n")
        with open(agent[:-len(".jsonl")] + ".meta.json", "w") as f:
            f.write('{"name":"done","toolUseId":"toolu_A"}')
        with open(os.path.join(td, "proj", "sess1.jsonl"), "w") as f:
            f.write(_tr("toolu_A"))
        old = os.stat(agent).st_mtime - 1800
        os.utime(agent, (old, old))
        global PROJECTS_DIR
        saved_projects = PROJECTS_DIR
        try:
            PROJECTS_DIR = td
            sess = [_mk_session(id="sess1")]
            subs = read_subagents(sess, now, default_config(), {})
        finally:
            PROJECTS_DIR = saved_projects
        assert len(subs) == 1 and subs[0].stopped, subs
        assert not subs[0].lingering(default_config().lingering_subagent_after)
        assert sess[0].subagents_active == 0

    print("selftest ok")


# ===== 5. CLI =====

def _acquire_lock(config_root: str) -> int:
    os.makedirs(config_root, exist_ok=True)
    lock_path = os.path.join(config_root, "lock")
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        print(f"[steward] another instance holds {lock_path}; exiting", file=sys.stderr)
        sys.exit(1)
    return fd


def main() -> int:
    p = argparse.ArgumentParser(description="claude-steward Python reference (prototype tier)")
    p.add_argument("--tick", action="store_true", help="alias for --once: run a single decide+act pass")
    p.add_argument("--enable-compact", action="store_true", help="Phase 2: real send-keys/socket actuation")
    p.add_argument("--enable-gate", action="store_true", help="Phase 1: publish the fan-out gate")
    p.add_argument("--enable-notify", action="store_true", help="explicit notify-on (already the default)")
    p.add_argument("--config-root", default=os.path.expanduser("~/.claude/steward"),
                    help="base dir for budget.json/gate.json/actions.jsonl/pause/ignore/lock")
    p.add_argument("--selftest", action="store_true", help="run the policy decision table and exit")
    p.add_argument("--once", action="store_true", help="run a single tick and exit")
    p.add_argument("--interval", type=float, default=30.0, help="loop interval in seconds (default 30)")
    args = p.parse_args()

    if args.selftest:
        selftest()
        return 0

    cfg = default_config()
    cfg.enable_compact = cfg.enable_compact or args.enable_compact
    cfg.enable_rules_gate = cfg.enable_rules_gate or args.enable_gate
    cfg.enable_notify = cfg.enable_notify or args.enable_notify

    _acquire_lock(args.config_root)
    dry_run = not args.enable_compact

    if args.tick or args.once:
        run_tick(cfg, args.config_root, dry_run)
        return 0

    print(f"[steward] looping every {args.interval}s (dry_run={dry_run}); Ctrl-C to stop")
    try:
        while True:
            run_tick(cfg, args.config_root, dry_run)
            time.sleep(args.interval)
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
