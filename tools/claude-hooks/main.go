package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-hook-fast <subcommand>")
		os.Exit(2)
	}

	subcommand := flag.Arg(0)

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
	case "post-hearth-loom":
		cmdPostHearthLoom()
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
	case "register-with-hearth-loom":
		cmdRegisterWithHearthLoom()
	case "deregister":
		cmdDeregister()
	case "steering-poll":
		cmdSteeringPoll()
	// Ports of Node/bash hooks (see cmd_cleanup_tmp.go, cmd_tengine_guards.go).
	// Replaces interpreter cold-start cost that compounds under loomie load.
	case "cleanup-tmp":
		cmdCleanupTmp()
	case "hardcoded-constants-guard":
		cmdHardcodedConstantsGuard()
	case "shader-hardcoded-ids-guard":
		cmdShaderHardcodedIDsGuard()
	case "shader-compile-validator":
		cmdShaderCompileValidator()
	case "flow":
		cmdFlowTelemetry()
	case "rules":
		cmdRules()
	case "session-init":
		cmdSessionInit()
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand")
		os.Exit(2)
	}
}
