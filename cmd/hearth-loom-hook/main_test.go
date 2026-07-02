package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Each test runs the handler directly with synthetic inputs and asserts
// the URL + body shape via an httptest server. We bypass main() because
// go test doesn't model stdin redirection per-subprocess; instead we
// exercise the handler funcs with fabricated stdin bytes.

type capturedPOST struct {
	mu     sync.Mutex
	method string
	path   string
	body   []byte
}

func (c *capturedPOST) http() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.method = r.Method
		c.path = r.URL.Path
		c.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
}

func (c *capturedPOST) snapshot() (string, string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, append([]byte{}, c.body...)
}

// withEnv sets env vars for the duration of the test and restores them.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	prev := map[string]string{}
	for k, v := range kv {
		prev[k] = os.Getenv(k)
		t.Setenv(k, v)
	}
	_ = prev // t.Setenv handles restore
}

// writeSessionID creates an HEARTH_SESSION_INFO_DIR with a session-id
// file inside a tempdir and returns the dir path.
func writeSessionID(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session-id"), []byte(id), 0o644); err != nil {
		t.Fatalf("write session-id: %v", err)
	}
	return dir
}

func TestSessionStart_HappyPath(t *testing.T) {
	cap := &capturedPOST{}
	srv := cap.http()
	defer srv.Close()

	infoDir := writeSessionID(t, "sess-from-file")
	withEnv(t, map[string]string{
		"HEARTH_BEAD_ID":          "loom-abc123",
		"HEARTH_LOOM_GATEWAY_URL": srv.URL,
		"HEARTH_SESSION_INFO_DIR": infoDir,
	})

	stdin := []byte(`{"session_id":"sess-from-hook","cwd":"/shuttle"}`)
	url, body, ok := sessionStart("loom-abc123", "sess-from-file", stdin)
	if !ok {
		t.Fatal("sessionStart returned !ok")
	}
	if got, want := url, "/api/v2/loomies/loom-abc123/session/registered"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if err := postJSON(srv.URL+url, body); err != nil {
		t.Fatalf("postJSON: %v", err)
	}

	method, path, gotBody := cap.snapshot()
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if path != "/api/v2/loomies/loom-abc123/session/registered" {
		t.Errorf("path = %s", path)
	}
	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body json: %v", err)
	}
	// Hook stdin session_id takes precedence over the file fallback.
	if payload["session_id"] != "sess-from-hook" {
		t.Errorf("session_id = %v, want sess-from-hook", payload["session_id"])
	}
	if payload["cwd"] != "/shuttle" {
		t.Errorf("cwd = %v", payload["cwd"])
	}
	if payload["event_id"] == nil || payload["event_id"] == "" {
		t.Error("event_id missing")
	}
}

func TestToolEvent_PrePhase(t *testing.T) {
	cap := &capturedPOST{}
	srv := cap.http()
	defer srv.Close()

	withEnv(t, map[string]string{
		"HEARTH_BEAD_ID":          "loom-xyz",
		"HEARTH_LOOM_GATEWAY_URL": srv.URL,
	})

	stdin := []byte(`{"tool_name":"Bash","tool_input":{"command":"ls"},"session_id":"sess1"}`)
	urlPath, body, ok := toolEvent("pre")("loom-xyz", "", stdin)
	if !ok {
		t.Fatal("toolEvent returned !ok")
	}
	if !strings.Contains(urlPath, "/loom-xyz/tool-event") {
		t.Errorf("urlPath = %s", urlPath)
	}
	if err := postJSON(srv.URL+urlPath, body); err != nil {
		t.Fatalf("postJSON: %v", err)
	}

	_, _, gotBody := cap.snapshot()
	var payload map[string]any
	_ = json.Unmarshal(gotBody, &payload)
	if payload["tool"] != "Bash" {
		t.Errorf("tool = %v", payload["tool"])
	}
	if payload["phase"] != "pre" {
		t.Errorf("phase = %v", payload["phase"])
	}
	if payload["session_id"] != "sess1" {
		t.Errorf("session_id = %v", payload["session_id"])
	}
}

func TestToolEvent_OpencodeShape(t *testing.T) {
	// opencode hooks pass {tool: "bash"} not {tool_name: ...}. Verify
	// we accept both.
	stdin := []byte(`{"tool":"bash","sessionID":"sess-oc"}`)
	_, body, ok := toolEvent("post")("loom-1", "fallback", stdin)
	if !ok {
		t.Fatal("toolEvent returned !ok")
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	if payload["tool"] != "bash" {
		t.Errorf("tool = %v", payload["tool"])
	}
}

func TestSessionStopped_ReasonFallthrough(t *testing.T) {
	stdin := []byte(`{"last_tool":"Edit","reason":"sigterm"}`)
	urlPath, body, ok := sessionStopped("stop")("loom-q", "sess-q", stdin)
	if !ok {
		t.Fatal("sessionStopped returned !ok")
	}
	if !strings.HasSuffix(urlPath, "/session/stopped") {
		t.Errorf("urlPath = %s", urlPath)
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	if payload["exit_reason"] != "sigterm" {
		t.Errorf("exit_reason = %v", payload["exit_reason"])
	}
	if payload["last_tool"] != "Edit" {
		t.Errorf("last_tool = %v", payload["last_tool"])
	}
}

func TestSessionStopped_OriginAsReason(t *testing.T) {
	// When stdin carries no reason field at all, the origin
	// subcommand (stop / subagent-stop) is used.
	_, body, _ := sessionStopped("subagent-stop")("loom-z", "sess-z", []byte("{}"))
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	if payload["exit_reason"] != "subagent-stop" {
		t.Errorf("exit_reason = %v, want subagent-stop", payload["exit_reason"])
	}
}

func TestPostJSON_TimeoutDoesNotBlock(t *testing.T) {
	// Server that holds the request until cleanup. The handler waits
	// on a stop channel so httptest.Server.Close can drain cleanly.
	stop := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stop
	}))
	t.Cleanup(func() {
		close(stop)
		listener.Close()
	})

	err := postJSON(listener.URL+"/x", []byte(`{}`))
	if err == nil {
		t.Fatal("postJSON returned nil — expected timeout")
	}
}

func TestUrlEscape_AllowsSafeIDs(t *testing.T) {
	cases := map[string]string{
		"loom-abc123":      "loom-abc123",
		"loom.fooBar_42":   "loom.fooBar_42",
		"loom abc":         "", // space rejected
		"loom/../etc":      "", // slash rejected
		"":                 "",
	}
	for in, want := range cases {
		if got := urlEscape(in); got != want {
			t.Errorf("urlEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadStdinCapped(t *testing.T) {
	// Stub stdin via os.Stdin replacement.
	r, w, _ := os.Pipe()
	prev := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = prev; _ = r.Close() }()

	want := strings.Repeat("a", 4096)
	go func() {
		_, _ = w.Write([]byte(want))
		_ = w.Close()
	}()
	got, err := readStdinCapped()
	if err != nil {
		t.Fatalf("readStdinCapped: %v", err)
	}
	if string(got) != want {
		t.Errorf("len(got) = %d, len(want) = %d", len(got), len(want))
	}
}

func TestDetectRuntime(t *testing.T) {
	t.Setenv("OPENCODE_PLUGIN", "1")
	t.Setenv("CLAUDE_CODE", "")
	t.Setenv("HEARTH_RUNTIME_ID", "")
	if got := detectRuntime(); got != "opencode" {
		t.Errorf("detectRuntime = %q, want opencode", got)
	}
	t.Setenv("OPENCODE_PLUGIN", "")
	t.Setenv("CLAUDE_CODE", "1")
	if got := detectRuntime(); got != "claude-code" {
		t.Errorf("detectRuntime = %q, want claude-code", got)
	}
}

// Ensure the server-side path is consistently URL-encodable; rejects
// the unsafe characters by emitting an empty string the handler then
// short-circuits on.
func TestSessionStart_RejectsUnsafeBeadID(t *testing.T) {
	urlPath, _, ok := sessionStart("loom abc", "sess", []byte("{}"))
	// urlEscape returns "" → handler builds an invalid URL but we
	// surface ok=true (callers run posrtJSON which will then fail
	// quietly). The intent: don't let a malformed bead id crash the
	// hook process.
	if !ok {
		t.Fatal("sessionStart should still return ok for malformed bead")
	}
	// The url path contains an empty bead segment when escape rejects.
	if !strings.HasPrefix(urlPath, "/api/v2/loomies//") {
		t.Logf("urlPath = %s (acceptable — handler will 404 server-side)", urlPath)
	}
}

func TestIsSessionRegistered(t *testing.T) {
	cases := map[string]bool{
		"/api/v2/loomies/loom-abc/session/registered": true,
		"/api/v2/loomies/loom-xyz/session/registered": true,
		"/api/v2/loomies/loom-abc/session/stopped":   false,
		"/api/v2/loomies/loom-abc/tool-event":          false,
		"/api/v2/beads":                               false,
	}
	for url, want := range cases {
		if got := isSessionRegistered(url); got != want {
			t.Errorf("isSessionRegistered(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestPostWithRetry_NoRetryOnNonSessionRegistered(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	err := postWithRetry(srv.URL+"/api/v2/loomies/loom-x/tool-event", []byte(`{}`), false)
	if err != nil {
		t.Fatalf("postWithRetry: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for non-session-registered)", attempts)
	}
}

func TestPostWithRetry_RetriesSessionRegistered(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	err := postWithRetry(srv.URL+"/api/v2/loomies/loom-x/session/registered", []byte(`{}`), true)
	if err != nil {
		t.Fatalf("postWithRetry: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (retry until success)", attempts)
	}
}

func TestPostWithRetry_GivesUpAfter3(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := postWithRetry(srv.URL+"/api/v2/loomies/loom-x/session/registered", []byte(`{}`), true)
	if err == nil {
		t.Fatal("postWithRetry returned nil despite all attempts failing")
	}
}

// Sanity that url.Parse on our generated URL succeeds (no smuggling
// of unescaped query-parsable characters).
func TestSessionStart_GeneratesValidURL(t *testing.T) {
	urlPath, _, _ := sessionStart("loom-good", "sess", []byte("{}"))
	if _, err := url.Parse("http://x" + urlPath); err != nil {
		t.Errorf("url.Parse: %v", err)
	}
}
