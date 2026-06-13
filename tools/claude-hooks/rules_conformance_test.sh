#!/usr/bin/env bash
# rules_conformance_test.sh — conformance runner for the Go rules engine.
#
# Feeds the same 36 fixture payloads as flow_guard_test.sh to the Go binary
# built at /tmp/claude-hook-fast-rulestest, using the tengine hook-rules.json.
#
# Usage:
#   bash tools/claude-hooks/rules_conformance_test.sh [path/to/binary] [path/to/hook-rules.json]
#
# Arguments (both optional):
#   $1 — path to compiled binary (default: /tmp/claude-hook-fast-rulestest)
#   $2 — path to hook-rules.json (default: /home/eric/projects/tengine/.claude/hook-rules.json
#         or the file in the agent-aca8bf25b5ec77aea worktree if the main-tree copy is absent)
#
# The CLAUDE_HOOK_RULES_PATH env var is set for every binary invocation so that
# config discovery is pinned rather than cwd-walking.
#
# Exit: 0 if all tests pass, 1 if any fail.

set -euo pipefail

BINARY="${1:-/tmp/claude-hook-fast-rulestest}"
RULES_JSON="${2:-}"

# Build the binary if it doesn't exist yet.
if [[ ! -x "$BINARY" ]]; then
    echo "Binary not found at $BINARY — building..."
    SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
    LOOM_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
    go build -o "$BINARY" "$LOOM_ROOT/tools/claude-hooks/"
fi

# Discover hook-rules.json.
if [[ -z "$RULES_JSON" ]]; then
    # Try main tree first, then the worktree that authored the file.
    TENGINE_ROOT="${TENGINE_MAIN_ROOT:-/home/eric/projects/tengine}"
    if [[ -f "$TENGINE_ROOT/.claude/hook-rules.json" ]]; then
        RULES_JSON="$TENGINE_ROOT/.claude/hook-rules.json"
    elif [[ -f "$TENGINE_ROOT/.claude/worktrees/agent-aca8bf25b5ec77aea/.claude/hook-rules.json" ]]; then
        RULES_JSON="$TENGINE_ROOT/.claude/worktrees/agent-aca8bf25b5ec77aea/.claude/hook-rules.json"
    fi
fi

if [[ -z "$RULES_JSON" ]] || [[ ! -f "$RULES_JSON" ]]; then
    echo "ERROR: hook-rules.json not found. Pass path as \$2 or set TENGINE_MAIN_ROOT."
    exit 1
fi

# Extract the project dir (parent of .claude/).
RULES_PROJECT_DIR="$(dirname "$(dirname "$RULES_JSON")")"

echo "Binary:      $BINARY"
echo "Rules:       $RULES_JSON"
echo "ProjectDir:  $RULES_PROJECT_DIR"
echo ""

PASS=0
FAIL=0
test_num=0
TEST_SESSION_BASE="conformance-$$"

# Every binary invocation gets CLAUDE_HOOK_RULES_PATH pinned to skip cwd walking,
# and TENGINE_MAIN_ROOT so the cargo_no_rust_edits / bash_write_guard builtins
# resolve the correct worktree base.
TENGINE_MAIN_ROOT="${TENGINE_MAIN_ROOT:-/home/eric/projects/tengine}"
export CLAUDE_HOOK_RULES_PATH="$RULES_JSON"
export TENGINE_MAIN_ROOT

run_test() {
    local name="$1"
    local expected_exit="$2"
    local json="$3"
    local extra_env="${4:-}"

    test_num=$((test_num + 1))
    local sid="${TEST_SESSION_BASE}-${test_num}"
    local patched
    patched=$(printf '%s' "$json" | jq --arg s "$sid" '.session_id = $s')

    local actual_exit=0
    if [[ -n "$extra_env" ]]; then
        env $extra_env "$BINARY" rules <<< "$patched" >/dev/null 2>&1 || actual_exit=$?
    else
        "$BINARY" rules <<< "$patched" >/dev/null 2>&1 || actual_exit=$?
    fi

    if [[ "$actual_exit" -eq "$expected_exit" ]]; then
        echo "  PASS [$test_num] $name"
        PASS=$((PASS + 1))
    else
        echo "  FAIL [$test_num] $name — expected exit $expected_exit, got $actual_exit"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== rules engine conformance test suite (36 tests from flow_guard_test.sh) ==="
echo ""

# ──────────────────────────────────────────────────────────────────────────────
# DETECTOR (a): log-fishing
# ──────────────────────────────────────────────────────────────────────────────
echo "--- Detector (a): log-fishing ---"

# [1] 1st shadergen start | grep → ALLOW
run_test "log-fishing: 1st shadergen start | grep → allow" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full | grep WARNING"},"cwd":"/tmp"}'

# [2] 2nd occurrence in same session → BLOCK
sid_lf="${TEST_SESSION_BASE}-lf"
test_num=$((test_num + 1))
patched_lf=$(jq --arg s "$sid_lf" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full | grep WARNING"},"cwd":"/tmp"}')
"$BINARY" rules <<< "$patched_lf" >/dev/null 2>&1 || true
actual_exit=0
"$BINARY" rules <<< "$patched_lf" >/dev/null 2>&1 || actual_exit=$?
if [[ "$actual_exit" -eq 2 ]]; then
    echo "  PASS [$test_num] log-fishing: 2nd occurrence → block"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] log-fishing: 2nd occurrence → expected exit 2, got $actual_exit"
    FAIL=$((FAIL + 1))
fi

# [3] shadergen start | tee → ALLOW (not grep)
run_test "log-fishing: shadergen start ... 2>&1 | tee → allow (not grep)" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full 2>&1 | tee /tmp/start-foo.log"},"cwd":"/tmp"}'

# [4] shadergen reload | grep → ALLOW (not start)
run_test "log-fishing: shadergen reload | grep → allow (not start)" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"shadergen reload | grep WARN"},"cwd":"/tmp"}'

# ──────────────────────────────────────────────────────────────────────────────
# DETECTOR (b): restart-loop (warn@3, block@6)
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Detector (b): restart-loop (warn@3, block@6) ---"

# [5] 3rd shadergen start → WARN (exit 0, non-blocking nudge since 2026-06-13)
sid_rl="${TEST_SESSION_BASE}-rl"
test_num=$((test_num + 1))
cmd_rl=$(jq --arg s "$sid_rl" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full"},"cwd":"/tmp"}')
for i in 1 2; do "$BINARY" rules <<< "$cmd_rl" >/dev/null 2>&1 || true; done
actual_exit=0
"$BINARY" rules <<< "$cmd_rl" >/dev/null 2>&1 || actual_exit=$?
if [[ "$actual_exit" -eq 0 ]]; then
    echo "  PASS [$test_num] restart-loop: 3rd shadergen start → warn (exit 0, non-blocking nudge)"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] restart-loop: 3rd shadergen start → expected exit 0 (warn), got $actual_exit"
    FAIL=$((FAIL + 1))
fi

# [6] 4th start → ALLOW
test_num=$((test_num + 1))
actual_exit=0
"$BINARY" rules <<< "$cmd_rl" >/dev/null 2>&1 || actual_exit=$?
if [[ "$actual_exit" -eq 0 ]]; then
    echo "  PASS [$test_num] restart-loop: 4th shadergen start → allow (past warn, before block)"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] restart-loop: 4th shadergen start → expected exit 0, got $actual_exit"
    FAIL=$((FAIL + 1))
fi

# [7] 6th start → BLOCK
test_num=$((test_num + 1))
"$BINARY" rules <<< "$cmd_rl" >/dev/null 2>&1 || true  # 5th
actual_exit=0
"$BINARY" rules <<< "$cmd_rl" >/dev/null 2>&1 || actual_exit=$?  # 6th
if [[ "$actual_exit" -eq 2 ]]; then
    echo "  PASS [$test_num] restart-loop: 6th shadergen start → block"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] restart-loop: 6th shadergen start → expected exit 2, got $actual_exit"
    FAIL=$((FAIL + 1))
fi

# [8] 3rd silo_tester → WARN (exit 0, non-blocking nudge since 2026-06-13)
sid_st="${TEST_SESSION_BASE}-st"
test_num=$((test_num + 1))
cmd_st=$(jq --arg s "$sid_st" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"silo_tester race_full --benchmark"},"cwd":"/tmp"}')
for i in 1 2; do "$BINARY" rules <<< "$cmd_st" >/dev/null 2>&1 || true; done
actual_exit=0
"$BINARY" rules <<< "$cmd_st" >/dev/null 2>&1 || actual_exit=$?
if [[ "$actual_exit" -eq 0 ]]; then
    echo "  PASS [$test_num] restart-loop: 3rd silo_tester → warn (exit 0, non-blocking nudge)"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] restart-loop: 3rd silo_tester → expected exit 0 (warn), got $actual_exit"
    FAIL=$((FAIL + 1))
fi

# [9] 2nd shadergen start → ALLOW
sid_rl2="${TEST_SESSION_BASE}-rl2"
test_num=$((test_num + 1))
cmd_rl2=$(jq --arg s "$sid_rl2" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full"},"cwd":"/tmp"}')
"$BINARY" rules <<< "$cmd_rl2" >/dev/null 2>&1 || true  # 1st
actual_exit=0
"$BINARY" rules <<< "$cmd_rl2" >/dev/null 2>&1 || actual_exit=$?  # 2nd
if [[ "$actual_exit" -eq 0 ]]; then
    echo "  PASS [$test_num] restart-loop: 2nd shadergen start → allow"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] restart-loop: 2nd shadergen start → expected exit 0, got $actual_exit"
    FAIL=$((FAIL + 1))
fi

# ──────────────────────────────────────────────────────────────────────────────
# DETECTOR (c): sleep-loop on GPU socket/lock
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Detector (c): sleep-loop on GPU socket/lock ---"

# [10] until ... shadergen socket → BLOCK
run_test "sleep-loop: until...sleep on shadergen socket → block" 2 \
    '{"tool_name":"Bash","tool_input":{"command":"until [ -S /tmp/shadergen-race_full.sock ]; do sleep 15; done"},"cwd":"/tmp"}'

# [11] while ... gpu-verify lock → BLOCK
run_test "sleep-loop: while...sleep on tengine-gpu-verify.lock → block" 2 \
    '{"tool_name":"Bash","tool_input":{"command":"while [ -f /tmp/tengine-gpu-verify.lock ]; do sleep 5; done"},"cwd":"/tmp"}'

# [12] until loop without GPU socket/lock → ALLOW
run_test "sleep-loop: until...sleep on unrelated path → allow" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"until [ -f /tmp/some-other.lock ]; do sleep 2; done"},"cwd":"/tmp"}'

# [13] bare sleep → ALLOW
run_test "sleep-loop: bare sleep command → allow" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"sleep 5"},"cwd":"/tmp"}'

# ──────────────────────────────────────────────────────────────────────────────
# DETECTOR (d): cargo-in-shader-task
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Detector (d): cargo-in-shader-task ---"

WT_CWD="/home/eric/projects/tengine/.claude/worktrees/wf_1bbd4985-e12-1"

# [14] cargo build in a worktree with no Rust edits → BLOCK
run_test "cargo-in-shader: cargo build, worktree cwd, no .rs edits → block" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"cargo build\"},\"cwd\":\"$WT_CWD\"}"

# [15] cargo build in main repo → ALLOW
run_test "cargo-in-shader: cargo build, main-tree cwd → allow" 0 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"cargo build\"},\"cwd\":\"$TENGINE_MAIN_ROOT\"}"

# [16] cargo build in /tmp → ALLOW
run_test "cargo-in-shader: cargo build, /tmp cwd → allow" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"cargo build"},"cwd":"/tmp"}'

# [17] cargo check → blocked in worktree with no edits
run_test "cargo-in-shader: cargo check, worktree cwd, no edits → block" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"cargo check --workspace\"},\"cwd\":\"$WT_CWD\"}"

# ──────────────────────────────────────────────────────────────────────────────
# DETECTOR (e): bash-write-guard
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Detector (e): bash-write-guard ---"

# [18] sed -i to main tree from worktree → BLOCK
run_test "bash-write-guard: sed -i main tree from worktree → block" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"sed -i 's/foo/bar/' $TENGINE_MAIN_ROOT/CLAUDE.md\"},\"cwd\":\"$WT_CWD\"}"

# [19] sed -i to worktree path → ALLOW
run_test "bash-write-guard: sed -i worktree path → allow" 0 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"sed -i 's/foo/bar/' $WT_CWD/some/file.rs\"},\"cwd\":\"$WT_CWD\"}"

# [20] tee to main tree → BLOCK
run_test "bash-write-guard: tee to main tree from worktree → block" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"echo hello | tee $TENGINE_MAIN_ROOT/output.txt\"},\"cwd\":\"$WT_CWD\"}"

# [21] redirect > to main tree → BLOCK
run_test "bash-write-guard: redirect > to main tree from worktree → block" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"echo x > $TENGINE_MAIN_ROOT/foo.txt\"},\"cwd\":\"$WT_CWD\"}"

# [22] git -C main-tree commit → BLOCK
run_test "bash-write-guard: git -C main-tree commit from worktree → block" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"git -C $TENGINE_MAIN_ROOT commit -m 'oops'\"},\"cwd\":\"$WT_CWD\"}"

# [23] cp with main-tree path as SOURCE → ALLOW
run_test "bash-write-guard: cp main-tree-src /tmp/dst → allow (source is not destination)" 0 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"cp $TENGINE_MAIN_ROOT/CLAUDE.md /tmp/backup.md\"},\"cwd\":\"$WT_CWD\"}"

# [24] rsync with main-tree path as SOURCE → ALLOW
run_test "bash-write-guard: rsync main-tree-src /tmp/dst → allow (source is not destination)" 0 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"rsync -av $TENGINE_MAIN_ROOT/shaders/ /tmp/shader-backup/\"},\"cwd\":\"$WT_CWD\"}"

# [25] cp with main-tree path as DESTINATION → BLOCK
run_test "bash-write-guard: cp /tmp/src main-tree-dst → block (writing to main tree)" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"cp /tmp/myfile.rs $TENGINE_MAIN_ROOT/src/main.rs\"},\"cwd\":\"$WT_CWD\"}"

# [26] mv with main-tree path as DESTINATION → BLOCK
run_test "bash-write-guard: mv /tmp/src main-tree-dst → block (writing to main tree)" 2 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"mv /tmp/output.rs $TENGINE_MAIN_ROOT/crates/tengine-dgc-hal/src/output.rs\"},\"cwd\":\"$WT_CWD\"}"

# [27] sed -i from main-tree cwd → ALLOW (not a worktree)
run_test "bash-write-guard: sed -i from main-tree cwd → allow (not a worktree)" 0 \
    "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"sed -i 's/foo/bar/' $TENGINE_MAIN_ROOT/CLAUDE.md\"},\"cwd\":\"$TENGINE_MAIN_ROOT\"}"

# ──────────────────────────────────────────────────────────────────────────────
# DETECTOR (f): source-read streak
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Detector (f): source-read streak ---"

# [28] 25th consecutive grep → BLOCK
sid_rs="${TEST_SESSION_BASE}-rs"
cmd_grep=$(jq --arg s "$sid_rs" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"grep foo /some/file"},"cwd":"/tmp"}')
for i in $(seq 1 24); do "$BINARY" rules <<< "$cmd_grep" >/dev/null 2>&1 || true; done

test_num=$((test_num + 1))
actual_25=0
"$BINARY" rules <<< "$cmd_grep" >/dev/null 2>&1 || actual_25=$?
if [[ "$actual_25" -eq 2 ]]; then
    echo "  PASS [$test_num] read-streak: 25th consecutive grep → block"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] read-streak: 25th consecutive grep → expected exit 2, got $actual_25"
    FAIL=$((FAIL + 1))
fi

# [29] 26th grep → ALLOW (counter was reset)
test_num=$((test_num + 1))
actual_26=0
"$BINARY" rules <<< "$cmd_grep" >/dev/null 2>&1 || actual_26=$?
if [[ "$actual_26" -eq 0 ]]; then
    echo "  PASS [$test_num] read-streak: 26th grep (after reset) → allow"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] read-streak: 26th grep after reset → expected exit 0, got $actual_26"
    FAIL=$((FAIL + 1))
fi

# [30] Non-read command resets counter: 10 greps, then non-read, then 25 more → block at 25
sid_rr="${TEST_SESSION_BASE}-rr"
test_num=$((test_num + 1))
cmd_grep_rr=$(jq --arg s "$sid_rr" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"grep foo /some/file"},"cwd":"/tmp"}')
cmd_git_rr=$(jq --arg s "$sid_rr" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"git status"},"cwd":"/tmp"}')
for i in $(seq 1 10); do "$BINARY" rules <<< "$cmd_grep_rr" >/dev/null 2>&1 || true; done
"$BINARY" rules <<< "$cmd_git_rr" >/dev/null 2>&1 || true  # resets streak
for i in $(seq 1 24); do "$BINARY" rules <<< "$cmd_grep_rr" >/dev/null 2>&1 || true; done
actual_rr=0
"$BINARY" rules <<< "$cmd_grep_rr" >/dev/null 2>&1 || actual_rr=$?
if [[ "$actual_rr" -eq 2 ]]; then
    echo "  PASS [$test_num] read-streak: non-read command resets counter (25 greps after reset → block)"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] read-streak: counter reset → expected block at 25 after reset, got $actual_rr"
    FAIL=$((FAIL + 1))
fi

# ──────────────────────────────────────────────────────────────────────────────
# Bypass env var
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Bypass env var ---"

# [31] TENGINE_FLOW_GUARD_OFF=1 allows blocked command
sid_bp="${TEST_SESSION_BASE}-bp"
cmd_bp=$(jq --arg s "$sid_bp" '.session_id=$s' \
    <<< '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full"},"cwd":"/tmp"}')
for i in 1 2 3 4; do "$BINARY" rules <<< "$cmd_bp" >/dev/null 2>&1 || true; done
test_num=$((test_num + 1))
actual_bp=0
TENGINE_FLOW_GUARD_OFF=1 "$BINARY" rules <<< "$cmd_bp" >/dev/null 2>&1 || actual_bp=$?
if [[ "$actual_bp" -eq 0 ]]; then
    echo "  PASS [$test_num] bypass: TENGINE_FLOW_GUARD_OFF=1 allows blocked command"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] bypass: expected exit 0 with TENGINE_FLOW_GUARD_OFF=1, got $actual_bp"
    FAIL=$((FAIL + 1))
fi

# ──────────────────────────────────────────────────────────────────────────────
# Malformed / empty stdin
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Malformed/empty stdin ---"

# [32] empty stdin → exit 0
test_num=$((test_num + 1))
actual_empty=0
echo "" | "$BINARY" rules >/dev/null 2>&1 || actual_empty=$?
if [[ "$actual_empty" -eq 0 ]]; then
    echo "  PASS [$test_num] empty stdin → exit 0"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] empty stdin → expected exit 0, got $actual_empty"
    FAIL=$((FAIL + 1))
fi

# [33] malformed JSON → exit 0
test_num=$((test_num + 1))
actual_garbage=0
echo "not json at all {{{" | "$BINARY" rules >/dev/null 2>&1 || actual_garbage=$?
if [[ "$actual_garbage" -eq 0 ]]; then
    echo "  PASS [$test_num] malformed JSON → exit 0"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] malformed JSON → expected exit 0, got $actual_garbage"
    FAIL=$((FAIL + 1))
fi

# [34] partial JSON (no tool_name) → exit 0
test_num=$((test_num + 1))
actual_partial=0
echo '{"session_id":"abc"}' | "$BINARY" rules >/dev/null 2>&1 || actual_partial=$?
if [[ "$actual_partial" -eq 0 ]]; then
    echo "  PASS [$test_num] partial JSON (no tool_name) → exit 0"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] partial JSON → expected exit 0, got $actual_partial"
    FAIL=$((FAIL + 1))
fi

# [35] non-Bash tool (Read) → always allow
test_num=$((test_num + 1))
actual_nonbash=0
echo '{"session_id":"x","tool_name":"Read","tool_input":{"command":"shadergen start foo | grep ERROR"},"cwd":"/tmp"}' \
    | "$BINARY" rules >/dev/null 2>&1 || actual_nonbash=$?
if [[ "$actual_nonbash" -eq 0 ]]; then
    echo "  PASS [$test_num] non-Bash tool (Read) → exit 0"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] non-Bash tool → expected exit 0, got $actual_nonbash"
    FAIL=$((FAIL + 1))
fi

# ──────────────────────────────────────────────────────────────────────────────
# Latency measurement: 200 sequential benign invocations
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Latency measurement (200 sequential benign calls) ---"
BENCH_JSON='{"session_id":"bench-latency","tool_name":"Bash","tool_input":{"command":"ls /tmp"},"cwd":"/tmp"}'
START_NS=$(date +%s%N)
for i in $(seq 1 200); do
    "$BINARY" rules <<< "$BENCH_JSON" >/dev/null 2>&1 || true
done
END_NS=$(date +%s%N)
ELAPSED_MS=$(( (END_NS - START_NS) / 1000000 ))
AVG_TENTHS=$(( (ELAPSED_MS * 10) / 200 ))
AVG_INT=$(( AVG_TENTHS / 10 ))
AVG_FRAC=$(( AVG_TENTHS % 10 ))
echo "  Total: ${ELAPSED_MS}ms for 200 calls"
echo "  Average: ${AVG_INT}.${AVG_FRAC}ms per call"

test_num=$((test_num + 1))
if [[ "$AVG_INT" -lt 10 ]]; then
    echo "  PASS [$test_num] avg ${AVG_INT}.${AVG_FRAC}ms < 10ms target"
    PASS=$((PASS + 1))
else
    echo "  WARN [$test_num] avg ${AVG_INT}.${AVG_FRAC}ms >= 10ms target (acceptable under load)"
    PASS=$((PASS + 1))  # latency is hardware-dependent, never a hard fail
fi

# ──────────────────────────────────────────────────────────────────────────────
# REGRESSION: quoted-string blindness + dead bypass (2026-06-12 lockout)
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- Regression: quoted mentions + command-text bypass ---"

# Quoted MENTION of shadergen start must not match the invocation detectors,
# even repeated past both counter thresholds (was: counted + blocked at 3/6).
sid_qm="${TEST_SESSION_BASE}-quoted-mention"
qm_payload=$(jq --arg s "$sid_qm" '.session_id=$s' <<< '{"tool_name":"Bash","tool_input":{"command":"python3 brief.py --decision \"agents must use shadergen start race_full and silo_tester foo sparingly\""},"cwd":"/tmp"}')
qm_fail=0
for rep in 1 2 3 4 5 6 7; do
    rc=0
    "$BINARY" rules <<< "$qm_payload" >/dev/null 2>&1 || rc=$?
    [[ "$rc" -ne 0 ]] && qm_fail=$rep && break
done
test_num=$((test_num + 1))
if [[ "$qm_fail" -eq 0 ]]; then
    echo "  PASS [$test_num] quoted 'shadergen start' mention x7 → never counted/blocked"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] quoted mention blocked on repetition $qm_fail"
    FAIL=$((FAIL + 1))
fi

# Single-quoted mention of the sleep-loop pattern → allow.
run_test "quoted sleep-loop prose → allow" 0 \
    '{"tool_name":"Bash","tool_input":{"command":"echo '\''never do: while true; do sleep 5; done on shadergen-foo.sock'\''"},"cwd":"/tmp"}'

# Real invocation still matches: drive a fresh session to the warn threshold.
# The 3rd start fires the non-blocking warn (exit 0); the counter still
# increments, so the 6th would still hard-block (covered by [7]).
sid_real="${TEST_SESSION_BASE}-real-start"
real_payload=$(jq --arg s "$sid_real" '.session_id=$s' <<< '{"tool_name":"Bash","tool_input":{"command":"shadergen start race_full"},"cwd":"/tmp"}')
"$BINARY" rules <<< "$real_payload" >/dev/null 2>&1 || true
"$BINARY" rules <<< "$real_payload" >/dev/null 2>&1 || true
rc=0
"$BINARY" rules <<< "$real_payload" >/dev/null 2>&1 || rc=$?
test_num=$((test_num + 1))
if [[ "$rc" -eq 0 ]]; then
    echo "  PASS [$test_num] real shadergen start still counted → warn at 3rd (exit 0, non-blocking)"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] real shadergen start 3rd → expected exit 0 (warn), got $rc"
    FAIL=$((FAIL + 1))
fi

# Command-text bypass: same session past threshold + TENGINE_FLOW_GUARD_OFF=1
# as an env prefix on the vetted command → allow (was: dead code, only the
# hook process env was consulted).
bypass_payload=$(jq --arg s "$sid_real" '.session_id=$s' <<< '{"tool_name":"Bash","tool_input":{"command":"TENGINE_FLOW_GUARD_OFF=1 shadergen start race_full"},"cwd":"/tmp"}')
rc=0
"$BINARY" rules <<< "$bypass_payload" >/dev/null 2>&1 || rc=$?
test_num=$((test_num + 1))
if [[ "$rc" -eq 0 ]]; then
    echo "  PASS [$test_num] TENGINE_FLOW_GUARD_OFF=1 command prefix bypasses past-threshold block"
    PASS=$((PASS + 1))
else
    echo "  FAIL [$test_num] command-prefix bypass → expected exit 0, got $rc"
    FAIL=$((FAIL + 1))
fi

# ──────────────────────────────────────────────────────────────────────────────
# Summary
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "==================================="
echo "Results: $PASS passed, $FAIL failed (of $test_num tests)"
echo "==================================="

[[ "$FAIL" -eq 0 ]]
