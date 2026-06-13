// Package client provides a low-latency HTTP client tuned for hook
// invocations.
//
// Properties:
//
//   - Single shared client per process, keepalive enabled. Hooks fire
//     in rapid succession; reusing the TCP connection turns a 30ms
//     hook into a 0.5ms hook on the second-and-onwards calls.
//   - 1.5s total budget (timeout) per call — caller can override via
//     PostJSONCtx if it has a tighter budget.
//   - All errors returned typed. Callers are expected to log + swallow
//     (host hooks must never block the runtime).
package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultTimeout is the per-call budget. Tuned for the host-loopback
// daemon (5908). Override at call-site for tighter budgets.
const DefaultTimeout = 1500 * time.Millisecond

// Shared is the package-level keepalive HTTP client. Safe for
// concurrent use; reuses the underlying transport's idle pool.
var Shared = &http.Client{
	Timeout: DefaultTimeout,
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

// ErrNon2xx wraps the server status text for callers that want to
// distinguish transport errors from API errors.
var ErrNon2xx = errors.New("non-2xx response")

// PostJSON POSTs the body to url with the DefaultTimeout. Returns an
// error on any failure (timeout, non-2xx, transport). Callers are
// expected to swallow this — hooks never exit non-zero.
func PostJSON(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	return PostJSONCtx(ctx, url, body)
}

// PostJSONCtx POSTs the body to url using the caller-supplied context.
// Useful when the caller has its own per-event budget that's tighter
// than DefaultTimeout.
func PostJSONCtx(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hearth-loom-host-hook/1")
	resp, err := Shared.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be returned to the keepalive pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return errors.New("hook POST " + url + ": " + resp.Status)
	}
	return nil
}
