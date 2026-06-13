package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

// withStdin pipes the given JSON into os.Stdin for the duration of fn.
// Each subcommand reads stdin once, so the pipe is closed after fn returns.
func withStdin(t *testing.T, payload string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = prev
		_ = r.Close()
	}()
	done := make(chan struct{})
	go func() {
		_, _ = io.WriteString(w, payload)
		_ = w.Close()
		close(done)
	}()
	fn()
	<-done
}

type capture struct {
	mu    sync.Mutex
	calls int32
	last  *http.Request
	body  []byte
}

func newServer(t *testing.T) (*capture, *httptest.Server) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.calls, 1)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.last = r
		c.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

// pluckProjectFields verifies the (project, worktree, display) trio
// every emission must carry. Returns the parsed envelope for further
// assertions.
func pluckProjectFields(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body json: %v", err)
	}
	return env
}

func TestCmdRegister_PostsCanonicalProject(t *testing.T) {
	// Build a fake projects root so Resolve produces deterministic
	// (canonical, worktree) values regardless of where the test runs.
	root := t.TempDir()
	wt := filepath.Join(root, "hearth-loom", "main")
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	c, srv := newServer(t)
	t.Setenv("HEARTH_LOOM_URL", srv.URL)
	t.Setenv("HEARTH_LOOM_ADDR", "") // ensure default URL wins

	payload := `{"session_id":"sess-1","cwd":"` + wt + `"}`
	withStdin(t, payload, cmdRegisterWithHearthLoom)

	if atomic.LoadInt32(&c.calls) != 1 {
		t.Fatalf("expected 1 POST, got %d", c.calls)
	}
	if got := c.last.URL.Path; got != "/api/v2/ccm/register" {
		t.Errorf("path = %q", got)
	}
	env := pluckProjectFields(t, c.body)
	if env["project"] != "hearth-loom" {
		t.Errorf("project = %v, want hearth-loom", env["project"])
	}
	if env["worktree"] != "main" {
		t.Errorf("worktree = %v, want main", env["worktree"])
	}
	if env["display"] != "hearth-loom [main]" {
		t.Errorf("display = %v, want hearth-loom [main]", env["display"])
	}
}

func TestCmdDeregister_PostsToHookEndpoint(t *testing.T) {
	c, srv := newServer(t)
	t.Setenv("HEARTH_LOOM_URL", srv.URL)

	// flag.Args needs the subcommand + event name. Simulate by
	// invoking the function directly with os.Args set up like the
	// dispatcher would; this also verifies the SubagentStop variant.
	payload := `{"session_id":"sess-9"}`

	// Re-parse flags within the test scope. The package uses the
	// global flag.CommandLine, so we need to reset it.
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"claude-hook-fast", "deregister", "SubagentStop"}
	// flag.Parse was already called once in production main(); but
	// tests don't call main. We rely on flag.NArg/Arg using the
	// CommandLine state. Re-call.
	resetFlags()

	withStdin(t, payload, cmdDeregister)

	if atomic.LoadInt32(&c.calls) != 1 {
		t.Fatalf("expected 1 POST, got %d", c.calls)
	}
	if got := c.last.URL.Path; got != "/api/v2/sessions/sess-9/hook/SubagentStop" {
		t.Errorf("path = %q", got)
	}
}

func TestCmdDeregister_DefaultsToStop(t *testing.T) {
	c, srv := newServer(t)
	t.Setenv("HEARTH_LOOM_URL", srv.URL)
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"claude-hook-fast", "deregister"}
	resetFlags()
	withStdin(t, `{"session_id":"sess-a"}`, cmdDeregister)
	if atomic.LoadInt32(&c.calls) != 1 {
		t.Fatalf("expected 1 POST, got %d", c.calls)
	}
	if got := c.last.URL.Path; got != "/api/v2/sessions/sess-a/hook/Stop" {
		t.Errorf("path = %q", got)
	}
}

func TestCmdSteeringPoll_404IsSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HEARTH_LOOM_URL", srv.URL)

	// Should NOT panic, should NOT fail. We're just exercising the
	// 404 path so the scaffolding survives until the endpoint lands.
	withStdin(t, `{"session_id":"sess-x"}`, cmdSteeringPoll)
}

func TestSteeringTargetID(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
		session    string
		wantID     string
		wantSub    bool
	}{
		{
			name:       "parent top-level transcript falls back to session",
			transcript: "/home/eric/.claude/projects/-home-eric-x/55483a8f-56b4-4839-906f-867bc19505f5.jsonl",
			session:    "55483a8f-56b4-4839-906f-867bc19505f5",
			wantID:     "55483a8f-56b4-4839-906f-867bc19505f5",
			wantSub:    false,
		},
		{
			name:       "subagent transcript yields bare hex",
			transcript: "/home/eric/.claude/projects/-home-eric-x/55483a8f-.../subagents/agent-a73d636e2f3dd42d7.jsonl",
			session:    "55483a8f-56b4-4839-906f-867bc19505f5",
			wantID:     "a73d636e2f3dd42d7",
			wantSub:    true,
		},
		{
			name:       "workflows-nested subagent transcript yields bare hex",
			transcript: "/home/eric/.claude/projects/-home-eric-x/abc/subagents/workflows/wf_32996959-38e/agent-a6a1969617cb47768.jsonl",
			session:    "abc",
			wantID:     "a6a1969617cb47768",
			wantSub:    true,
		},
		{
			name:       "empty transcript falls back to session",
			transcript: "",
			session:    "sess-x",
			wantID:     "sess-x",
			wantSub:    false,
		},
		{
			name:       "subagents dir but non-agent basename falls back",
			transcript: "/x/subagents/workflows/wf_1/journal.jsonl",
			session:    "sess-y",
			wantID:     "sess-y",
			wantSub:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotSub := steeringTargetID(tc.transcript, tc.session)
			if gotID != tc.wantID || gotSub != tc.wantSub {
				t.Errorf("steeringTargetID(%q, %q) = (%q, %v), want (%q, %v)",
					tc.transcript, tc.session, gotID, gotSub, tc.wantID, tc.wantSub)
			}
		})
	}
}

func TestBoundedToolSummary(t *testing.T) {
	long := strings.Repeat("a", 800)
	raw := []byte(`{"command":"` + long + `"}`)
	got := boundedToolSummary(raw)
	if len(got) > 200 {
		t.Errorf("len = %d, want ≤ 200", len(got))
	}
	if got := boundedToolSummary(nil); got != "" {
		t.Errorf("empty input should yield empty summary, got %q", got)
	}
}

func TestBaseEnvelope_StampsWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "hearth-loom", "main")
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEARTH_HOOK_PROJECTS_ROOT", root)

	in := event.HookInput{SessionID: "s-1", CWD: wt, TranscriptPath: "/tmp/conv-zz.jsonl"}
	env, can := baseEnvelope(in, "started", "host_claude_code")
	if can != "hearth-loom" {
		t.Errorf("canonical = %q", can)
	}
	if env["project"] != "hearth-loom" || env["worktree"] != "main" {
		t.Errorf("env = %+v", env)
	}
	if env["display"] != "hearth-loom [main]" {
		t.Errorf("display = %v", env["display"])
	}
	if env["conversation_id"] != "conv-zz" {
		t.Errorf("conversation_id = %v", env["conversation_id"])
	}
}

// resetFlags rebuilds flag.CommandLine and calls flag.Parse so per-test
// changes to os.Args are picked up by flag.Arg / flag.NArg. The
// production binary calls flag.Parse() exactly once in main().
func resetFlags() {
	// We're using the global flag set. Re-parse from the current
	// os.Args[1:] so flag.Arg(N) reflects the test's args.
	parseFlagsForTest()
}
