// Package nbuspub shells out to the `nervous publish` CLI for host
// hooks that need to land an event directly on nervous-bus without a
// daemon round-trip.
//
// This is the PostToolUse → bus.agent.activity.v1 path: the
// hearth-loom daemon endpoint /api/v2/sessions/{sid}/hook/{event}
// intentionally does NOT route PostToolUse (volume + latency budget),
// so the host hook publishes directly. Mirrors the contract of the
// legacy bash hook (~/.claude/hooks/post-tool-use/zzy-nbus-activity.sh)
// for drop-in replacement.
//
// Failure modes — all silent:
//   - `nervous` CLI not on PATH (fresh worktree, fresh shell) → skip
//   - publish exits non-zero (bus down, redis offline)            → skip
//   - context deadline exceeded                                   → skip
//
// Writes a diagnostic line via hooklog.Writef when the publish errors;
// the operator can grep for it if the bus appears stale.
package nbuspub

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

// SourceTag is the NERVOUS_SOURCE value the host hook brands every
// event with. The nervous CLI defaults to /<basename-cwd>; without the
// override every host-hook event would carry the current project's
// name which makes cross-project rollups noisy. /claude-host/<project>
// is the convention the bash hooks established and dashboards rely on.
//
// Callers pass the project name; we splice it under /claude-host/.
func SourceTag(project string) string {
	if project == "" {
		project = "unknown"
	}
	return "/claude-host/" + project
}

// PublishTimeout bounds the `nervous publish` child process. Since the
// hook hot paths moved to PublishAsync (detached worker), this bounds
// BACKGROUND delivery, not hook latency — so it can afford headroom.
// At the old 2s, 159 of 200 recent publishes died `signal: killed`
// under loomie load (nervous spawns 3-4 python interpreters per event);
// 5s recovers delivery without any hook ever waiting on it.
const PublishTimeout = 5 * time.Second

// WorkerSubcommand is the hidden argv[1] both hook binaries register for
// the detached publish worker spawned by PublishAsync.
const WorkerSubcommand = "nbus-publish-worker"

// PublishAsync appends a buffered receipt and starts a detached worker with
// null stdio. A successful return is NOT a durability acknowledgement: the
// worker syncs the inherited descriptor before sending to Redis. Start errors
// leave the buffered receipt for the mirror, without synchronous CLI fallback.
func PublishAsync(channel, source, payload string) error {
	return publishAsync(channel, source, payload, os.Executable, StartDetached)
}

func publishAsync(channel, source, payload string, executable func() (string, error), start func(*exec.Cmd) error) error {
	raw, err := prepareEnvelope(channel, source, payload)
	if err != nil {
		return err
	}
	receipt, err := openDebugReceipt(raw)
	if err != nil {
		return fmt.Errorf("nbus-async receipt: %w", err)
	}
	defer func() { _ = receipt.Close() }()
	exe, err := executable()
	if err != nil {
		return fmt.Errorf("nbus-async executable: %w", err)
	}
	// fd 3 keeps the appended inode alive across rotation. Reopening the path
	// in the worker could sync an unrelated, newly created debug.jsonl.
	cmd := exec.Command(exe, WorkerSubcommand, receiptWorkerMode, "3", string(raw))
	cmd.ExtraFiles = []*os.File{receipt}
	if err := start(cmd); err != nil {
		return fmt.Errorf("nbus-async start: %w", err)
	}
	return nil
}

// StartDetached starts cmd in its own session with null stdio and reaps it
// from a background goroutine. Process.Release() is NOT a reap: in a
// long-lived caller (the codex-hook lease-worker fires one publish per 30s
// refresh) every Release()d child stayed a zombie for the caller's whole
// lifetime — ~270 defunct codex-hook processes measured 2026-08-31. The
// goroutine Wait clears each child within seconds; short-lived hook callers
// exit first and their children reparent to init/subreaper exactly as before.
func StartDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// WorkerMain syncs an inherited receipt before exact-envelope delivery.
// The legacy channel/source/payload form retains its CLI fallback.
func WorkerMain(args []string) {
	if len(args) != 3 {
		return
	}
	if args[0] == receiptWorkerMode {
		if args[1] != "3" {
			return
		}
		receipt := os.NewFile(3, "nbus-receipt")
		defer func() { _ = receipt.Close() }()
		if err := syncAndDeliverReceipt(receipt, []byte(args[2])); err != nil {
			hooklog.Writef("nbus-async worker err=%v", err)
		}
		return
	}
	// Legacy direct-worker form; new callers use the exact-envelope form.
	if err := publishDirect(args[0], args[1], args[2], false); err != nil {
		hooklog.Writef("nbus-direct fallback channel=%s err=%v", args[0], err)
		_ = Publish(args[0], args[1], args[2])
	}
}

// Publish shells out to `nervous publish <channel> <payload>` with
// the supplied NERVOUS_SOURCE override and a hard timeout. Returns
// nil on success or a swallowable error on failure. Callers should
// log via hooklog and exit zero — host hooks never propagate errors.
func Publish(channel, source, payload string) error {
	if _, err := exec.LookPath("nervous"); err != nil {
		// CLI not installed (fresh container, minimal shell) — silent.
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), PublishTimeout)
	defer cancel()

	// #nosec G204 — channel + payload are constructed by our own code,
	// not by the hook stdin. The stdin-derived fields are JSON-escaped
	// inside payload before reaching here.
	cmd := exec.CommandContext(ctx, "nervous", "publish", channel, payload)
	cmd.Env = childEnv(source)
	// Detach stdio: nervous prints chatter we don't want surfacing
	// through the hook (Claude Code captures it as a hook failure
	// signal even when exit code is 0).
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		hooklog.Writef("nbus-publish channel=%s err=%v", channel, err)
		return err
	}
	return nil
}

func childEnv(source string) []string {
	// We import os here lazily so the package can be tested without
	// touching real env in unit tests (Publish itself shells out, so
	// tests should mock by setting an alternative PATH).
	env := osEnviron()
	out := env[:0]
	const prefix = "NERVOUS_SOURCE="
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	// Disable zellij plugin invocation. The bash hook does this for
	// the same reason: zellij cleanup adds 0.5-1s and we already
	// bounded the publish at 2s.
	hasNoZellij := false
	const zk = "NERVOUS_NO_ZELLIJ="
	for _, kv := range out {
		if len(kv) >= len(zk) && kv[:len(zk)] == zk {
			hasNoZellij = true
			break
		}
	}
	if !hasNoZellij {
		out = append(out, "NERVOUS_NO_ZELLIJ=1")
	}
	out = append(out, prefix+source)
	return out
}
