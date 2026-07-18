package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

// --- frame encode/decode round-trip ---

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	f1 := encodeFrame(1, []byte(`{"t":"pong"}`))
	f2 := encodeFrame(0, []byte("/compact\r"))
	f3 := encodeFrame(1, []byte{}) // zero-length payload must round-trip too

	var buf []byte
	buf = append(buf, f1...)
	buf = append(buf, f2...)
	buf = append(buf, f3...)

	frames, err := decodeFrames(buf)
	if err != nil {
		t.Fatalf("decodeFrames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(frames))
	}
	if frames[0].Kind != 1 || string(frames[0].Payload) != `{"t":"pong"}` {
		t.Errorf("frame0 = %+v", frames[0])
	}
	if frames[1].Kind != 0 || string(frames[1].Payload) != "/compact\r" {
		t.Errorf("frame1 = %+v", frames[1])
	}
	if frames[2].Kind != 1 || len(frames[2].Payload) != 0 {
		t.Errorf("frame2 = %+v", frames[2])
	}
}

func TestEncodeFrameHeaderBytes(t *testing.T) {
	f := encodeFrame(1, []byte("ab"))
	// 4-byte BE length (2) + kind byte (1) + payload ("ab").
	want := []byte{0, 0, 0, 2, 1, 'a', 'b'}
	if string(f) != string(want) {
		t.Errorf("encodeFrame header/payload = %v, want %v", f, want)
	}
}

// --- tmux safety gate ---

type fakeRunCmd struct {
	calls   [][]string
	outputs map[string]string // joined argv -> output
	err     error
}

func (f *fakeRunCmd) run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	if f.err != nil {
		return "", f.err
	}
	if out, ok := f.outputs[strings.Join(argv, " ")]; ok {
		return out, nil
	}
	return "", nil
}

func newTestOSActuator(t *testing.T, fr *fakeRunCmd) *osActuator {
	t.Helper()
	dir := t.TempDir()
	return &osActuator{
		runCmd:     fr.run,
		dialUnix:   func(context.Context, string) (net.Conn, error) { return nil, errors.New("not implemented") },
		auditPath:  filepath.Join(dir, "actions.jsonl"),
		pausePath:  filepath.Join(dir, "pause"),
		rosterPath: filepath.Join(dir, "roster.json"),
	}
}

func TestCompactTmux_NonClaudePaneAborts(t *testing.T) {
	fr := &fakeRunCmd{outputs: map[string]string{
		"tmux display -p -t %3 #{pane_current_command}": "bash\n",
	}}
	a := newTestOSActuator(t, fr)
	s := steward.SessionState{ID: "sess1", PaneID: "tmux:%3"}

	err := a.Compact(context.Background(), s, steward.MethodTmux, "test")
	if err == nil {
		t.Fatal("expected error for non-claude pane, got nil")
	}
	if len(fr.calls) != 1 {
		t.Fatalf("expected exactly 1 runCmd call (the display check), got %d: %v", len(fr.calls), fr.calls)
	}
}

func TestCompactTmux_ClaudePaneSendsExactSequence(t *testing.T) {
	fr := &fakeRunCmd{outputs: map[string]string{
		"tmux display -p -t %3 #{pane_current_command}": "claude\n",
	}}
	a := newTestOSActuator(t, fr)
	s := steward.SessionState{ID: "sess1", PaneID: "tmux:%3"}

	if err := a.Compact(context.Background(), s, steward.MethodTmux, "pressure"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	want := [][]string{
		{"tmux", "display", "-p", "-t", "%3", "#{pane_current_command}"},
		{"tmux", "send-keys", "-t", "%3", "-l", "/compact"},
		{"tmux", "send-keys", "-t", "%3", "Enter"},
	}
	if len(fr.calls) != len(want) {
		t.Fatalf("call count = %d, want %d: %v", len(fr.calls), len(want), fr.calls)
	}
	for i, argv := range want {
		if strings.Join(fr.calls[i], "|") != strings.Join(argv, "|") {
			t.Errorf("call[%d] = %v, want %v", i, fr.calls[i], argv)
		}
	}

	// Audit log should have two records: sent=false then sent=true.
	data, err := os.ReadFile(a.auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 audit lines, got %d: %s", len(lines), data)
	}
	var rec0, rec1 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec0); err != nil {
		t.Fatalf("unmarshal audit[0]: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &rec1); err != nil {
		t.Fatalf("unmarshal audit[1]: %v", err)
	}
	if rec0["sent"] != false || rec1["sent"] != true {
		t.Errorf("audit sent flags = %v, %v; want false, true", rec0["sent"], rec1["sent"])
	}
	if rec0["method"] != "tmux" || rec0["session_id"] != "sess1" {
		t.Errorf("audit rec0 = %+v", rec0)
	}
}

func TestCompactTmux_NotTmuxPaneAborts(t *testing.T) {
	fr := &fakeRunCmd{}
	a := newTestOSActuator(t, fr)

	cases := []steward.SessionState{
		{ID: "sess1", PaneID: ""},
		{ID: "sess1", PaneID: "zellij:s:p"},
	}
	for _, s := range cases {
		err := a.Compact(context.Background(), s, steward.MethodTmux, "test")
		if err == nil {
			t.Errorf("PaneID=%q: expected error, got nil", s.PaneID)
		}
		if len(fr.calls) != 0 {
			t.Errorf("PaneID=%q: expected no runCmd calls, got %v", s.PaneID, fr.calls)
		}
	}
}

func TestCompact_PauseFileAborts(t *testing.T) {
	fr := &fakeRunCmd{outputs: map[string]string{
		"tmux display -p -t %3 #{pane_current_command}": "claude\n",
	}}
	a := newTestOSActuator(t, fr)
	if err := os.WriteFile(a.pausePath, []byte("paused"), 0o644); err != nil {
		t.Fatalf("write pause file: %v", err)
	}

	s := steward.SessionState{ID: "sess1", PaneID: "tmux:%3"}
	err := a.Compact(context.Background(), s, steward.MethodTmux, "test")
	if err == nil {
		t.Fatal("expected error when pause file present, got nil")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("expected no runCmd calls while paused, got %v", fr.calls)
	}
}

// --- atomic budget/gate writes ---

func TestFileBudgetWriter_AtomicValidJSON(t *testing.T) {
	dir := t.TempDir()
	w := &fileBudgetWriter{path: filepath.Join(dir, "budget.json")}
	doc := steward.BudgetDoc{State: "watch", UpdatedAt: "2026-07-18T00:00:00Z", FanoutGate: "warn"}

	if err := w.Write(context.Background(), doc); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatalf("read budget.json: %v", err)
	}
	var got steward.BudgetDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("budget.json is not valid JSON: %v", err)
	}
	if got.State != "watch" || got.FanoutGate != "warn" {
		t.Errorf("got = %+v", got)
	}

	// No leftover temp files in the directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestFileGateWriter_AtomicValidJSON(t *testing.T) {
	dir := t.TempDir()
	w := &fileGateWriter{path: filepath.Join(dir, "gate.json")}

	if err := w.SetGate(context.Background(), steward.FanoutBlock, steward.LevelCritical); err != nil {
		t.Fatalf("SetGate: %v", err)
	}

	data, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatalf("read gate.json: %v", err)
	}
	var got gateDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("gate.json is not valid JSON: %v", err)
	}
	if got.Gate != "block" || got.Level != "critical" {
		t.Errorf("got = %+v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.UpdatedAt); err != nil {
		t.Errorf("updated_at not RFC3339: %q (%v)", got.UpdatedAt, err)
	}
}

// --- ptysock handshake over net.Pipe ---

func writeTestCtrl(t *testing.T, conn net.Conn, msg ctrlMsg) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal ctrl: %v", err)
	}
	if _, err := conn.Write(encodeFrame(1, b)); err != nil {
		t.Fatalf("write ctrl frame: %v", err)
	}
}

func mustReadFrame(t *testing.T, r *bufio.Reader) pdFrame {
	t.Helper()
	f, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	return f
}

func TestCompactPtySock_HandshakeOverNetPipe(t *testing.T) {
	dir := t.TempDir()
	rosterPath := filepath.Join(dir, "roster.json")
	rosterJSON := `{"workers":{"w1":{"sessionId":"sess1","ptyAuth":"secret-token","ptySock":"/tmp/ignored.sock"}}}`
	if err := os.WriteFile(rosterPath, []byte(rosterJSON), 0o644); err != nil {
		t.Fatalf("write roster: %v", err)
	}

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	a := &osActuator{
		runCmd:     (&fakeRunCmd{}).run,
		dialUnix:   func(context.Context, string) (net.Conn, error) { return clientConn, nil },
		auditPath:  filepath.Join(dir, "actions.jsonl"),
		pausePath:  filepath.Join(dir, "pause"),
		rosterPath: rosterPath,
	}
	s := steward.SessionState{ID: "sess1", PtySock: "/tmp/ignored.sock"}

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.Compact(context.Background(), s, steward.MethodPtySock, "test")
	}()

	serverBuf := bufio.NewReader(serverConn)

	// 1. client's initial pong.
	f := mustReadFrame(t, serverBuf)
	assertCtrl(t, f, "pong", "")

	// 2. client's auth with the roster's ptyAuth.
	f = mustReadFrame(t, serverBuf)
	assertCtrl(t, f, "auth", "secret-token")

	// 3. server -> hello (discarded by client).
	writeTestCtrl(t, serverConn, ctrlMsg{T: "hello"})

	// 4. server -> screen replay (kind-0, discarded by client).
	if _, err := serverConn.Write(encodeFrame(0, []byte("replay-bytes"))); err != nil {
		t.Fatalf("write replay frame: %v", err)
	}

	// 5. server -> ping; client must reply pong before continuing.
	writeTestCtrl(t, serverConn, ctrlMsg{T: "ping"})
	f = mustReadFrame(t, serverBuf)
	assertCtrl(t, f, "pong", "")

	// 6. server -> live; client's handshake loop returns.
	writeTestCtrl(t, serverConn, ctrlMsg{T: "live"})

	// 7. client injects the compact keystroke as one kind-0 data frame.
	f = mustReadFrame(t, serverBuf)
	if f.Kind != 0 || string(f.Payload) != "/compact\r" {
		t.Fatalf("inject frame = %+v, want kind=0 payload=/compact\\r", f)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Compact returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Compact did not return in time")
	}
}

func assertCtrl(t *testing.T, f pdFrame, wantT, wantToken string) {
	t.Helper()
	if f.Kind != 1 {
		t.Fatalf("frame kind = %d, want 1 (control)", f.Kind)
	}
	var msg ctrlMsg
	if err := json.Unmarshal(f.Payload, &msg); err != nil {
		t.Fatalf("unmarshal control frame: %v", err)
	}
	if msg.T != wantT {
		t.Fatalf("control t = %q, want %q", msg.T, wantT)
	}
	if wantToken != "" && msg.Token != wantToken {
		t.Fatalf("control token = %q, want %q", msg.Token, wantToken)
	}
}
