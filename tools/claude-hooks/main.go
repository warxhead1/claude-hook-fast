package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/nbuspub"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/timing"
)

// hookStart/hookCmd feed the invocation-timing ledger. os.Exit skips
// defers, so guard exit paths route through hookExit instead of os.Exit
// to keep exit-2 invocations in the timings too.
var (
	hookStart = time.Now()
	hookCmd   string
)

func hookExit(code int) {
	timing.Record("claude-hook-fast", hookCmd, time.Since(hookStart))
	traceFinish(code)
	os.Exit(code)
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-hook-fast <subcommand>")
		os.Exit(2)
	}

	subcommand := flag.Arg(0)
	hookCmd = subcommand
	traceInit(subcommand)

	switch subcommand {
	case "tmux-preparing":
		cmdTmuxPreparing()
	case "plan-tracker":
		cmdPlanTracker()
	case "agent-heartbeat":
		cmdAgentHeartbeat()
	case "feedback-collector":
		cmdFeedbackCollector()
	case "tmux-ready":
		cmdTmuxReady()
	case "tmux-notification":
		cmdTmuxNotification()
	case "tmux-thinking":
		cmdTmuxThinking()
	// Bash-to-Go migration: nbus emitters. See cmd_nbus.go.
	case "nbus-activity":
		cmdNbusActivity()
	case "nbus-session-start":
		cmdNbusSessionStart()
	case "nbus-stop":
		cmdNbusStop()
	case "nbus-notification":
		cmdNbusNotification()
	case "nbus-pre-compact":
		cmdNbusPreCompact()
	case "nbus-permission-request":
		cmdNbusPermissionRequest()
	case "nbus-message-display":
		cmdNbusMessageDisplay()
	case "nbus-stop-failure":
		cmdNbusStopFailure()
	case "nbus-session-end":
		cmdNbusSessionEnd()
	case "nbus-subagent-lifecycle":
		cmdNbusSubagentLifecycle()
	case "steering-poll":
		cmdSteeringPoll()
	// Ports of Node/bash hooks (see cmd_cleanup_tmp.go, cmd_tengine_guards.go).
	// Replaces interpreter cold-start cost that compounds under loomie load.
	case "cleanup-tmp":
		cmdCleanupTmp()
	case "dispatch-model-guard":
		cmdDispatchModelGuard()
	case "hardcoded-constants-guard":
		cmdHardcodedConstantsGuard()
	case "shader-hardcoded-ids-guard":
		cmdShaderHardcodedIDsGuard()
	case "shader-compile-validator":
		cmdShaderCompileValidator()
	case "rules":
		cmdRules()
	case "session-init":
		cmdSessionInit()
	case "plan-resume":
		cmdPlanResume()
	case "memory-staleness":
		cmdMemoryStaleness()
	case "kb-prime-siblings":
		cmdKbPrimeSiblings()
	case "kb-inbox-capture":
		cmdKbInboxCapture()
	case "kb-dispatch-inject":
		cmdKbDispatchInject()
	case "kb-subagent-capture":
		cmdKbSubagentCapture()
	// Worktree-isolation guard family (ports of the retired
	// ~/.claude/hooks/pre-tool-use/*.py one-offs; see worktree_common.go).
	case "worktree-bash-guard":
		cmdWorktreeBashGuard()
	case "worktree-write-guard":
		cmdWorktreeWriteGuard()
	case "dispatch-freeze-guard":
		cmdDispatchFreezeGuard()
	// Claim-gate (T6 frozen contract): edit-without-verify gate. See
	// cmd_claim_gate.go.
	case "claim-gate-track":
		cmdClaimGateTrack()
	case "claim-gate":
		cmdClaimGate()
	// Bead-claim-gate: bonus companion catching claimed-and-abandoned
	// beads. See cmd_bead_claim_gate.go.
	case "bead-claim-gate-track":
		cmdBeadClaimGateTrack()
	case "bead-claim-gate":
		cmdBeadClaimGate()
	// Detached bus-publish worker (spawned by nbuspub.PublishAsync) and
	// the invocation-latency report. See internal/hooks/timing.
	case nbuspub.WorkerSubcommand:
		nbuspub.WorkerMain(flag.Args()[1:])
	case "timings":
		cmdTimings()
	// idle-stop-check (Stop): mechanical check for "ended the turn with
	// only a background waiter armed" — nervous-bus#36. See
	// cmd_idle_stop_check.go.
	case "idle-stop-check":
		cmdIdleStopCheck()
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand")
		os.Exit(2)
	}
	timing.Record("claude-hook-fast", subcommand, time.Since(hookStart))
	traceFinish(0)
}
