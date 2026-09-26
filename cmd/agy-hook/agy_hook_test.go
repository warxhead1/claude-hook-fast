package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// capturedPublish records one publishAsync call for assertion.
type capturedPublish struct {
	Channel, Source, Payload string
}

// clearPaneEnv makes pane/session-name capture hermetic: these tests run
// under `go test`, which may or may not inherit ZELLIJ_*/TMUX_PANE from
// the invoking shell, and the golden envelopes never carry them.
func clearPaneEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ZELLIJ_PANE_ID", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
}

func stubPublish(t *testing.T) *[]capturedPublish {
	t.Helper()
	clearPaneEnv(t)
	var calls []capturedPublish
	old := publishAsync
	publishAsync = func(channel, source, payload string) error {
		calls = append(calls, capturedPublish{channel, source, payload})
		return nil
	}
	t.Cleanup(func() { publishAsync = old })
	return &calls
}

// decodeEnvelope parses a captured payload into a plain map and strips
// "ts" (non-deterministic wall-clock value) so the remaining fields can
// be compared exactly against a golden expectation captured from the
// live Python script (see /home/eric/data2/orca/nervous-bus/_evidence/
// 2026-09-25/agy-port/scratch/goldens — Python 3.11.13, run with a fake
// `nervous` on PATH so publish() writes its argv straight to a capture
// file instead of touching the real bus).
func decodeEnvelope(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("invalid envelope JSON %q: %v", raw, err)
	}
	if _, ok := m["ts"]; !ok {
		t.Fatalf("envelope missing ts field: %v", m)
	}
	delete(m, "ts")
	return m
}

func assertEnvelope(t *testing.T, got, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("envelope mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// repoWorktree is the real worktree this test runs from
// (/home/eric/data2/worktrees/claude-hook-fast/<name>), whose .git file
// points at .../claude-hook-fast/.git/worktrees/<name> — this is the
// live analogue of the Python script's worktree-gitdir-parsing branch,
// exercised without any fixture fakery.
func repoWorktree(t *testing.T) (path, project, worktree string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // cmd/agy-hook -> repo root
	return root, "claude-hook-fast", filepath.Base(root)
}

func TestPostToolWithCommand_MatchesPythonGolden(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","transcriptPath":"/home/eric/.gemini/transcripts/conv-abc123.jsonl","toolCall":{"name":"RunCommand","args":{"CommandLine":"go test ./...","description":"run tests"}}}`
	runPostTool(strings.NewReader(payload))

	if len(*calls) != 1 {
		t.Fatalf("expected 1 publish, got %d: %+v", len(*calls), *calls)
	}
	c := (*calls)[0]
	if c.Channel != chanAgentActivity {
		t.Fatalf("channel = %q, want %q", c.Channel, chanAgentActivity)
	}
	if c.Source != "/claude-host/"+project {
		t.Fatalf("source = %q", c.Source)
	}
	got := decodeEnvelope(t, c.Payload)
	want := map[string]any{
		"event": "tool_call", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-abc123", "session_id": "conv-abc123",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"transcript_path": "/home/eric/.gemini/transcripts/conv-abc123.jsonl",
		"tool_name":       "RunCommand", "tool_summary": "go test ./...",
	}
	assertEnvelope(t, got, want)
}

func TestPostToolNoName_IsHeartbeat(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","transcriptPath":"/home/eric/.gemini/transcripts/conv-abc123.jsonl","toolCall":{}}`
	runPostTool(strings.NewReader(payload))

	if len(*calls) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(*calls))
	}
	got := decodeEnvelope(t, (*calls)[0].Payload)
	want := map[string]any{
		"event": "heartbeat", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-abc123", "session_id": "conv-abc123",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"transcript_path": "/home/eric/.gemini/transcripts/conv-abc123.jsonl",
	}
	assertEnvelope(t, got, want)
}

func TestPostToolError_AppendsSuffixAndSharesTruncationBudget(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-err1","modelName":"gemini-3-pro","transcriptPath":"/tmp/t.jsonl","toolCall":{"name":"EditFile","args":{"path":"/etc/hosts"}},"error":"permission denied"}`
	runPostTool(strings.NewReader(payload))

	got := decodeEnvelope(t, (*calls)[0].Payload)
	want := map[string]any{
		"event": "tool_call", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-err1", "session_id": "conv-err1",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"transcript_path": "/tmp/t.jsonl",
		"tool_name":       "EditFile",
		"tool_summary":    `{"path": "/etc/hosts"} [error: permission denied]`,
	}
	assertEnvelope(t, got, want)
}

// The Python script's fallback summary is json.dumps(args) — default
// separators (", ", ": "), original key order. This is the highest-risk
// divergence point for a naive Go port (encoding/json has no spaces by
// default), so it gets its own golden case.
func TestPostToolJSONArgsFallback_MatchesPythonDumpsSpacing(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-json1","modelName":"gemini-3-pro","toolCall":{"name":"ReadFile","args":{"path":"/etc/hosts","lines":100}}}`
	runPostTool(strings.NewReader(payload))

	got := decodeEnvelope(t, (*calls)[0].Payload)
	want := map[string]any{
		"event": "tool_call", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-json1", "session_id": "conv-json1",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"tool_name":    "ReadFile",
		"tool_summary": `{"path": "/etc/hosts", "lines": 100}`,
	}
	assertEnvelope(t, got, want)
}

func TestPreInvocationFirst_PublishesBothChannelsIdentically(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","transcriptPath":"/home/eric/.gemini/transcripts/conv-abc123.jsonl","invocationNum":1}`
	runPreInvocation(strings.NewReader(payload))

	if len(*calls) != 2 {
		t.Fatalf("expected 2 publishes, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].Channel != chanAgentActivity || (*calls)[1].Channel != chanAgentSession {
		t.Fatalf("unexpected channels: %+v", *calls)
	}
	want := map[string]any{
		"event": "started", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-abc123", "session_id": "conv-abc123",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"transcript_path": "/home/eric/.gemini/transcripts/conv-abc123.jsonl",
	}
	assertEnvelope(t, decodeEnvelope(t, (*calls)[0].Payload), want)
	assertEnvelope(t, decodeEnvelope(t, (*calls)[1].Payload), want)
}

func TestPreInvocationSecond_IsNoop(t *testing.T) {
	repo, _, _ := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","invocationNum":2}`
	runPreInvocation(strings.NewReader(payload))
	if len(*calls) != 0 {
		t.Fatalf("expected no publishes for invocationNum>1, got %+v", *calls)
	}
}

func TestStopPlain_PublishesEndedToBothChannels(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","transcriptPath":"/home/eric/.gemini/transcripts/conv-abc123.jsonl"}`
	runStop(strings.NewReader(payload))

	if len(*calls) != 2 {
		t.Fatalf("expected 2 publishes, got %d", len(*calls))
	}
	want := map[string]any{
		"event": "ended", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-abc123", "session_id": "conv-abc123",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"transcript_path": "/home/eric/.gemini/transcripts/conv-abc123.jsonl",
	}
	assertEnvelope(t, decodeEnvelope(t, (*calls)[0].Payload), want)
	assertEnvelope(t, decodeEnvelope(t, (*calls)[1].Payload), want)
}

func TestStopTermination_PassesReasonThroughVerbatim(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","terminationReason":"user_stop"}`
	runStop(strings.NewReader(payload))

	want := map[string]any{
		"event": "ended", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-abc123", "session_id": "conv-abc123",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"termination_reason": "user_stop",
	}
	assertEnvelope(t, decodeEnvelope(t, (*calls)[0].Payload), want)
	assertEnvelope(t, decodeEnvelope(t, (*calls)[1].Payload), want)
}

func TestStopError_StringifiesAndTruncatesTo200Runes(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	calls := stubPublish(t)
	payload := `{"workspacePaths":["` + repo + `"],"conversationId":"conv-abc123","modelName":"gemini-3-pro","error":"agent crashed unexpectedly"}`
	runStop(strings.NewReader(payload))

	want := map[string]any{
		"event": "ended", "agent": "antigravity", "agent_kind": "antigravity-cli",
		"agent_id": "conv-abc123", "session_id": "conv-abc123",
		"project": project, "worktree": worktree,
		"display": project + " [" + worktree + "]", "cwd": repo,
		"model": "gemini-3-pro", "source_kind": "antigravity-cli",
		"error": "agent crashed unexpectedly",
	}
	assertEnvelope(t, decodeEnvelope(t, (*calls)[0].Payload), want)
	assertEnvelope(t, decodeEnvelope(t, (*calls)[1].Payload), want)
}

// --- unit tests for the helpers golden coverage above depends on ---

func TestRepoSplit_WorktreeGitdirPointer(t *testing.T) {
	repo, project, worktree := repoWorktree(t)
	gotProject, gotWorktree := repoSplit(repo)
	if gotProject != project || gotWorktree != worktree {
		t.Fatalf("repoSplit(%q) = (%q, %q), want (%q, %q)", repo, gotProject, gotWorktree, project, worktree)
	}
}

func TestRepoSplit_DirectCheckoutGitDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project, worktree := repoSplit(dir)
	if project != filepath.Base(dir) || worktree != "" {
		t.Fatalf("repoSplit(%q) = (%q, %q)", dir, project, worktree)
	}
}

func TestRepoSplit_EmptyPathWalksFromCwd(t *testing.T) {
	// path="" mirrors `path or os.getcwd()`: the WALK starts at cwd (here,
	// the agy-hook-port worktree itself), so .git is found on the very
	// first iteration and the outer "no .git found anywhere" fallback
	// (which is keyed off the ORIGINAL, pre-substitution path argument,
	// not cwd) is never reached from a cwd that is itself inside a repo.
	repo, project, worktree := repoWorktree(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd != repo {
		t.Skipf("test cwd %q is not the worktree root %q; fallback-path behavior not exercised", wd, repo)
	}
	gotProject, gotWorktree := repoSplit("")
	if gotProject != project || gotWorktree != worktree {
		t.Fatalf("repoSplit(\"\") = (%q, %q), want (%q, %q)", gotProject, gotWorktree, project, worktree)
	}
}

func TestRepoSplit_FallbackUsesOriginalPathNotWalkedCwd(t *testing.T) {
	// Exercises the `os.path.basename(path or "") or "unknown"` line
	// directly: when no .git is ever found while walking up from a
	// filesystem-root-adjacent directory, the reported project name
	// comes from the ORIGINAL argument to repoSplit, not from wherever
	// the walk (which may have substituted os.Getwd()) ended up.
	dir := t.TempDir() // outside any git repo (data2 worktrees root has no .git)
	sub := filepath.Join(dir, "leaf")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	project, worktree := repoSplit(sub)
	if project != "leaf" || worktree != "" {
		t.Fatalf("repoSplit(%q) = (%q, %q), want (\"leaf\", \"\")", sub, project, worktree)
	}
}

func TestPyDumps_MatchesPythonJSONDumpsDefaultSpacing(t *testing.T) {
	args := orderedMap{
		{Key: "path", Value: "/etc/hosts"},
		{Key: "lines", Value: mustNumber(t, "100")},
	}
	got := pyDumps(args)
	want := `{"path": "/etc/hosts", "lines": 100}`
	if got != want {
		t.Fatalf("pyDumps = %q, want %q", got, want)
	}
}

func TestTruncateRunes_CutsByCodepointNotByte(t *testing.T) {
	// "é" is 2 bytes in UTF-8 but 1 rune; Python's s[:1] on "éx" yields
	// "é", not a mangled half-byte.
	got := truncateRunes("éx", 1)
	if got != "é" {
		t.Fatalf("truncateRunes = %q", got)
	}
}

func TestCompactEnvelope_DropsEmptyStringAndNilOnly(t *testing.T) {
	env := map[string]any{"a": "", "b": nil, "c": "kept", "d": 0, "e": false}
	got := compactEnvelope(env)
	want := map[string]any{"c": "kept", "d": 0, "e": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compactEnvelope = %#v, want %#v", got, want)
	}
}

func mustNumber(t *testing.T, s string) json.Number {
	t.Helper()
	return json.Number(s)
}
