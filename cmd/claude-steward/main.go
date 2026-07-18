// Command claude-steward is a rate-limit-aware session steward.
//
// # Purpose
//
// It runs as a small single-instance daemon that, once per tick, observes the
// live Claude Code runtime (usage cache, per-session state files, statusline
// context/pane files, the daemon roster, lingering subagent transcripts),
// decides — with the pure policy core in internal/steward — what the current
// pressure level is, and executes the effects that level warrants: writing the
// public budget.json contract, publishing a fan-out gate, notifying, and (only
// when explicitly enabled) compacting idle sessions that are near cache expiry.
//
// # Rollout ladder
//
// The four phase gates are OFF by default (pure observer): the daemon only
// writes budget.json and, with --enable-notify, surfaces rises. Each flag opens
// one rung of the ladder:
//
//	--enable-notify        Phase 0/1: emit human-facing notifications
//	--enable-gate          Phase 1: publish budget.json for the rules engine
//	--enable-compact       Phase 2: actually send /compact to eligible sessions
//	--enable-fanout-block  Phase 3: escalate the gate to block at CRITICAL
//
// # Safety
//
//   - Single instance only: a flock guarantees two stewards never race on
//     budget.json or double-fire compactions.
//   - Pause is a file (~/.claude/steward/pause). Its presence halts all
//     actuation on the very next tick — no restart needed — while budget.json
//     keeps refreshing so the operator still sees state.
//   - A read or per-action failure is logged and swallowed; the loop keeps
//     ticking. Only a failure to acquire the lock or build the executor is
//     fatal.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

func main() {
	var (
		interval    = flag.Duration("interval", 30*time.Second, "tick interval; the daemon observes and acts once per interval")
		once        = flag.Bool("once", false, "run a single tick and exit (implies no ticker; useful for cron or testing)")
		enableNote  = flag.Bool("enable-notify", false, "Phase 0/1: emit human-facing notifications on pressure rise")
		enableGate  = flag.Bool("enable-gate", false, "Phase 1: publish the fan-out gate document for the rules engine")
		enableCompt = flag.Bool("enable-compact", false, "Phase 2: send /compact to eligible idle sessions under pressure")
		enableBlock = flag.Bool("enable-fanout-block", false, "Phase 3: escalate the fan-out gate to block at CRITICAL")
		lockPath    = flag.String("lock", "", "single-instance lock file (default ~/.claude/steward/steward.lock)")
		pausePath   = flag.String("pause-file", "", "pause file whose presence halts all actuation (default ~/.claude/steward/pause)")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "claude-steward ", log.LstdFlags)

	stewardDir := defaultStewardDir()
	if *lockPath == "" {
		*lockPath = filepath.Join(stewardDir, "steward.lock")
	}
	if *pausePath == "" {
		*pausePath = filepath.Join(stewardDir, "pause")
	}

	// Single instance: a second steward would race on budget.json and could
	// double-fire compactions. Fail fast rather than block.
	lock, err := acquireLock(*lockPath)
	if err != nil {
		logger.Fatalf("cannot acquire lock: %v", err)
	}
	defer lock.release()

	// Build the config: start from the observer defaults, then open exactly the
	// gates the operator requested on the command line.
	cfg := steward.DefaultConfig()
	cfg.EnableNotify = *enableNote
	cfg.EnableRulesGate = *enableGate
	cfg.EnableCompact = *enableCompt
	cfg.EnableFanoutBlock = *enableBlock

	// Wire the concrete edges. Only the ones a phase needs are consulted, but we
	// always construct them — they hold no expensive resources, and the executor
	// nil-guards anything unused.
	exec := &executor{
		reader:   newFileReader(readerPaths{}),
		actuator: newOSActuator(),
		notifier: newBusNotifier(),
		budget:   newFileBudgetWriter(),
		gate:     newFileGateWriter(),
		state:    LoadState(filepath.Join(stewardDir, "state.json")),
		cfg:      cfg,
		clock:    time.Now,
		paused:   func() bool { return pauseFilePresent(*pausePath) },
		logf:     logger.Printf,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		exec.run(ctx, 0)
		return
	}

	logger.Printf("started: interval=%s notify=%t gate=%t compact=%t fanout-block=%t",
		*interval, cfg.EnableNotify, cfg.EnableRulesGate, cfg.EnableCompact, cfg.EnableFanoutBlock)
	exec.run(ctx, *interval)
	logger.Print("stopped")
}

// pauseFilePresent reports whether the pause file exists. Any stat error other
// than clear existence is treated as "not paused" so a transient FS hiccup
// never wedges actuation off; the operator's explicit `touch` is what pauses.
func pauseFilePresent(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
