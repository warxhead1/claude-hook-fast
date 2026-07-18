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

// PublishAsync spawns the current binary as a detached worker
// (`<self> nbus-publish-worker <channel> <source> <payload>`, own
// session, null stdio) and returns immediately. The hook path pays
// fork+exec (~5-10ms) instead of the full nervous/redis round-trip,
// which routinely exceeded the 2s hook budget Codex allows. Falls back
// to the synchronous Publish only when the re-exec itself fails.
func PublishAsync(channel, source, payload string) error {
	exe, err := os.Executable()
	if err != nil {
		return Publish(channel, source, payload)
	}
	// The worker rebuilds its own child env via Publish → childEnv; the
	// source tag rides argv, already wrapped by the caller's SourceTag.
	cmd := exec.Command(exe, WorkerSubcommand, channel, source, payload)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return Publish(channel, source, payload)
	}
	return cmd.Process.Release()
}

// WorkerMain is the detached-worker entry point. Direct Go delivery
// first (~2-5ms, zero interpreter spawns — see direct.go); the nervous
// CLI (bounded, hooklog-on-failure) only as fallback, e.g. an authed
// redis URL or an unusable payload.
func WorkerMain(args []string) {
	if len(args) != 3 {
		return
	}
	if err := PublishDirect(args[0], args[1], args[2]); err != nil {
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
