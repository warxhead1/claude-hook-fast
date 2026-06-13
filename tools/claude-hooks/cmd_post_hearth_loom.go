// cmd_post_hearth_loom.go — `claude-hook-fast post-hearth-loom <event>`
//
// Forwards a Claude Code hook payload to the hearth-loom gateway endpoint
//
//	POST /api/v2/sessions/{session-id}/hook/{event}
//
// which then fans out onto nervous-bus channels:
//
//	SessionStart  → bus.session.started.v1
//	Notification  → bus.session.notification.v1   ("Claude needs you" push)
//	Stop          → bus.session.stopped.v1
//	SubagentStop  → bus.session.subagent.stopped.v1
//
// Wire on the host with:
//
//	~/.claude/hooks/notification/zzz-hearth-loom.sh:
//	    #!/usr/bin/env bash
//	    exec claude-hook-fast post-hearth-loom Notification
//
// Latency budget: this hook fires on every Notification / Stop /
// SubagentStop / SessionStart. The HTTP client uses a process-lifetime
// keepalive transport (sharedHTTPClient) and a 1.5s overall timeout — the
// hook returns and unblocks Claude even when the daemon is down.
//
// Failure mode: ANY error (daemon offline, bad JSON, timeout) is silently
// swallowed. Claude Code hook conventions require zero-exit + no stderr
// chatter unless the hook itself wants to gate the action — and these
// events are observational, never gating.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// sharedHTTPClient is a package-level keepalive client so per-hook
// invocations of the post-hearth-loom subcommand don't pay a TCP +
// TLS handshake cost. The host fires these on every Notification /
// Stop / SubagentStop, so the connection pool measurably helps when a
// burst of subagents finish back-to-back.
var sharedHTTPClient = &http.Client{
	Timeout: 1500 * time.Millisecond,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   500 * time.Millisecond,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // payloads are tiny
	},
}

// hearthLoomBaseURL returns the gateway base URL.
// Delegates to hearthLoomURL() (cmd_nbus.go) which reads $HEARTH_LOOM_URL
// and defaults to 127.0.0.1:5908 — the canonical hearth-loom port.
func hearthLoomBaseURL() string { return hearthLoomURL() }

// cmdPostHearthLoom forwards stdin to the gateway hook endpoint.
//
// Usage: claude-hook-fast post-hearth-loom <event>
//
// <event> is the Anthropic hook name (SessionStart, Notification, Stop,
// SubagentStop, PostToolUse, …). The session_id is extracted from the
// stdin JSON payload; if absent the request is dropped (the gateway
// route requires a non-empty session_id path param).
func cmdPostHearthLoom() {
	// flag.Parse() already ran in main(); flag.Arg(0) is the subcommand
	// name ("post-hearth-loom") and flag.Arg(1) is the event name.
	if flag.NArg() < 2 {
		// Match the silent-on-misuse convention of the other subcommands.
		return
	}
	event := flag.Arg(1)
	if event == "" {
		return
	}

	// Read stdin with a hard cap matching the gateway's MaxBytesReader.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) == 0 {
		return
	}

	// Peek session_id. Lenient — extra fields ignored.
	var peek struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil || peek.SessionID == "" {
		appendHookLog(fmt.Sprintf("%s - post-hearth-loom[%s]: missing session_id, dropped\n", timestamp(), event))
		return
	}

	url := fmt.Sprintf("%s/api/v2/sessions/%s/hook/%s",
		hearthLoomBaseURL(), peek.SessionID, event)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-hook-fast/post-hearth-loom")

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		appendHookLog(fmt.Sprintf("%s - post-hearth-loom[%s]: %v\n", timestamp(), event, err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain to enable keepalive reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		appendHookLog(fmt.Sprintf("%s - post-hearth-loom[%s]: status=%d\n", timestamp(), event, resp.StatusCode))
	}
}
