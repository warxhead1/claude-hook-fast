package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const coordStubScript = `#!/bin/sh
D="$STUB_DIR"
echo "$*" >> "$D/calls.log"
case "$2" in
run-current) cat "$D/run_current.json"; exit 0;;
task-list) cat "$D/task_list.json"; exit 0;;
inbox) cat "$D/inbox.json" 2>/dev/null || echo '{"ok":true,"result":{"messages":[]}}'; exit 0;;
worker-show) cat "$D/worker_show.json" 2>/dev/null; exit 0;;
worker-list) cat "$D/worker_list.json" 2>/dev/null || echo '{"ok":true,"result":{"workers":[]}}'; exit 0;;
worker-stop) [ -f "$D/stopfail" ] && exit 1; echo '{"ok":true,"result":{}}'; exit 0;;
rm) echo '{"ok":true,"result":{}}'; exit 0;;
reply) [ -f "$D/replyfail" ] && exit 1; echo '{"ok":true,"result":{}}'; exit 0;;
check)
  [ -f "$D/fail" ] && exit 1
  case " $* " in *" --wait "*) [ -f "$D/wait" ] && sleep "$(cat "$D/wait")";; esac
  case " $* " in *" --wait "*) if [ -f "$D/waiterexists" ]; then rm "$D/waiterexists"; [ -f "$D/later" ] && mv "$D/later" "$D/q/999.json"; echo '{"ok":false,"error":{"code":"waiter_exists","message":"Run run_x already has an active actionable waiter."}}'; exit 1; fi;; esac
  n=$(ls "$D/q" 2>/dev/null | sort | head -1)
  if [ -n "$n" ]; then cat "$D/q/$n"; rm "$D/q/$n"; else echo '{"ok":true,"result":{"runId":"run_x","messages":[],"count":0}}'; fi
  exit 0;;
esac
exit 1
`

type coordHarness struct {
	t    *testing.T
	dir  string
	cfg  coordConfig
	errb *bytes.Buffer
	seq  int
}

func newCoordHarness(t *testing.T, coordinator string) *coordHarness {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "orca-stub")
	if err := os.WriteFile(stub, []byte(coordStubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "q"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := fmt.Sprintf(`{"ok":true,"result":{"run":{"id":"run_x","coordinator_handle":%q}}}`, coordinator)
	_ = os.WriteFile(filepath.Join(dir, "run_current.json"), []byte(rc), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "task_list.json"),
		[]byte(`{"ok":true,"result":{"tasks":[{"id":"task_a","task_title":"Hero build: spinner"}]}}`), 0o644)
	t.Setenv("STUB_DIR", dir)
	t.Setenv("COORD_WAKE_DISABLE", "")
	errb := &bytes.Buffer{}
	return &coordHarness{t: t, dir: dir, errb: errb, cfg: coordConfig{
		Handle: "term_coord", OrcaBin: stub, StateRoot: filepath.Join(dir, "state"),
		Slice: time.Second, MinCycle: 5 * time.Millisecond, WatchEvery: 50 * time.Millisecond,
		CacheTTL: time.Minute, Ancestors: []int{os.Getpid()}, Stderr: errb,
	}}
}

func coordMsgJSON(id, typ, from, subject, body string) string {
	m := map[string]any{"id": id, "run_id": "run_x", "from_handle": from, "to_handle": "run:run_x",
		"subject": subject, "body": body, "type": typ, "thread_id": nil,
		"payload": `{"taskId":"task_a","dispatchId":"ctx_1","phase":"waiting"}`, "created_at": "2026-10-05T00:00:00Z"}
	b, _ := json.Marshal(m)
	return string(b)
}

// enqueue adds one check response (a delivery batch) to the stub queue.
func (h *coordHarness) enqueue(delivery string, msgs ...string) {
	h.seq++
	body := fmt.Sprintf(`{"ok":true,"result":{"runId":"run_x","deliveryId":%q,"messages":[%s],"count":%d}}`,
		delivery, strings.Join(msgs, ","), len(msgs))
	p := filepath.Join(h.dir, "q", fmt.Sprintf("%03d.json", h.seq))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *coordHarness) calls() []string {
	b, _ := os.ReadFile(filepath.Join(h.dir, "calls.log"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (h *coordHarness) runDir() string { return filepath.Join(h.cfg.StateRoot, "run_x") }

func TestCoordWakeNoHandle(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.cfg.Handle = ""
	if got := coordWake(h.cfg); got != 0 {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "calls.log")); err == nil {
		t.Fatal("orca was called without a handle")
	}
}

func TestCoordWakeKillSwitch(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	t.Setenv("COORD_WAKE_DISABLE", "1")
	if got := coordWake(h.cfg); got != 0 {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "calls.log")); err == nil {
		t.Fatal("orca was called with kill switch set")
	}
}

func TestCoordWakeNotCoordinator(t *testing.T) {
	h := newCoordHarness(t, "term_someone_else")
	h.enqueue("d1", coordMsgJSON("msg_1", "question", "term_lane", "Q", "b"))
	if got := coordWake(h.cfg); got != 0 {
		t.Fatalf("exit %d", got)
	}
	if c := h.calls(); len(c) != 1 || !strings.Contains(c[0], "run-current") {
		t.Fatalf("calls beyond run-current: %v", c)
	}
	// Cached verdict: a second lane invocation makes no orca call at all.
	if got := coordWake(h.cfg); got != 0 {
		t.Fatalf("exit %d", got)
	}
	if c := h.calls(); len(c) != 1 {
		t.Fatalf("cache miss, calls: %v", c)
	}
}

func TestCoordWakeSingleton(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	if err := os.MkdirAll(h.runDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(h.runDir(), "wake.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if got := coordWake(h.cfg); got != 0 {
		t.Fatalf("exit %d", got)
	}
	for _, c := range h.calls() {
		if strings.Contains(c, "check") {
			t.Fatalf("second instance polled: %s", c)
		}
	}
}

func TestCoordWakeQuestionThenAck(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.enqueue("d1", coordMsgJSON("msg_q1", "question", "term_lane", "May I have a GPU window", "need render"))
	if got := coordWake(h.cfg); got != 2 {
		t.Fatalf("exit %d, want 2", got)
	}
	out := h.errb.String()
	for _, want := range []string{"msg_q1", "question", "May I have a GPU window", "need render",
		"orca-ide orchestration reply --id msg_q1", "Hero build: spinner"} {
		if !strings.Contains(out, want) {
			t.Errorf("wake block missing %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(h.runDir(), "pending-ack")); string(b) != "d1" {
		t.Fatalf("pending-ack = %q", b)
	}
	// Next run acks d1 on its first check, then wakes again on d2.
	h.errb.Reset()
	h.enqueue("d2", coordMsgJSON("msg_q2", "escalation", "term_lane", "stuck", "help"))
	if got := coordWake(h.cfg); got != 2 {
		t.Fatalf("second run exit %d", got)
	}
	var checks []string
	for _, c := range h.calls() {
		if strings.Contains(c, " check ") {
			checks = append(checks, c)
		}
	}
	if len(checks) != 2 || strings.Contains(checks[0], "--ack") || !strings.Contains(checks[1], "--ack d1") {
		t.Fatalf("ack not passed on the next run: %v", checks)
	}
	if b, _ := os.ReadFile(filepath.Join(h.runDir(), "pending-ack")); string(b) != "d2" {
		t.Fatalf("pending-ack after run two = %q", b)
	}
}

func TestCoordWakeTelemetryLedgerNoWake(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.enqueue("d1",
		coordMsgJSON("msg_s1", "status", "term_lane", "building hull", "70 percent"),
		coordMsgJSON("msg_h1", "heartbeat", "term_lane2", "alive", ""))
	h.enqueue("d2", coordMsgJSON("msg_q1", "question", "term_lane", "Q?", "b"))
	if got := coordWake(h.cfg); got != 2 {
		t.Fatalf("exit %d", got)
	}
	out := h.errb.String()
	if strings.Contains(out, "building hull") || strings.Contains(out, "msg_h1") {
		t.Fatalf("telemetry leaked into wake block:\n%s", out)
	}
	var checks []string
	for _, c := range h.calls() {
		if strings.Contains(c, " check ") {
			checks = append(checks, c)
		}
	}
	if len(checks) < 2 || !strings.Contains(checks[1], "--ack d1") {
		t.Fatalf("telemetry batch not acked: %v", checks)
	}
	b, err := os.ReadFile(filepath.Join(h.runDir(), "lanes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var l coordLedger
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	if l.Lanes["term_lane"] == nil || l.Lanes["term_lane"].LastStatus.Subject != "building hull" {
		t.Fatalf("status not in ledger: %s", b)
	}
	if l.Lanes["term_lane2"] == nil || l.Lanes["term_lane2"].LastHeartbeat == "" {
		t.Fatalf("heartbeat not in ledger: %s", b)
	}
	var sb, eb bytes.Buffer
	if rc := coordStatus(h.cfg.StateRoot, "", false, &sb, &eb); rc != 0 ||
		!strings.Contains(sb.String(), "building hull") || !strings.Contains(sb.String(), "alive") {
		t.Fatalf("coord-status rc=%d out=%q err=%q", rc, sb.String(), eb.String())
	}
}

func TestCoordWakeOrcaFailureExitsZero(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	_ = os.WriteFile(filepath.Join(h.dir, "fail"), nil, 0o644)
	if got := coordWake(h.cfg); got != 0 {
		t.Fatalf("exit %d", got)
	}
	if h.errb.Len() != 0 {
		t.Fatalf("stderr written on failure: %q", h.errb.String())
	}
}

func TestCoordWakeParentGone(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	_ = os.WriteFile(filepath.Join(h.dir, "wait"), []byte("20"), 0o644)
	parent := exec.Command("sleep", "30")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = parent.Wait(); close(reaped) }()
	defer func() { _ = parent.Process.Kill() }()
	h.cfg.Ancestors = []int{parent.Process.Pid}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = parent.Process.Kill()
	}()
	done := make(chan int, 1)
	go func() { done <- coordWake(h.cfg) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("exit %d", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("waiter outlived its parent")
	}
	<-reaped
}

func TestCoordFixturesParse(t *testing.T) {
	b, err := os.ReadFile("testdata/coord/check_peek.json")
	if err != nil {
		t.Fatal(err)
	}
	var r coordCheckResp
	if err := json.Unmarshal(b, &r); err != nil || !r.OK || len(r.Result.Messages) == 0 {
		t.Fatalf("check fixture: err=%v ok=%v n=%d", err, r.OK, len(r.Result.Messages))
	}
	types := map[string]bool{}
	for _, m := range r.Result.Messages {
		types[m.Type] = true
		if m.ID == "" || m.From == "" {
			t.Fatalf("message missing id/from: %+v", m)
		}
	}
	for _, want := range []string{"question", "status", "heartbeat", "worker_done"} {
		if !types[want] {
			t.Errorf("fixture lacks type %s", want)
		}
	}
	rb, err := os.ReadFile("testdata/coord/run_current.json")
	if err != nil {
		t.Fatal(err)
	}
	var rc struct {
		Result struct {
			Run struct {
				ID          string `json:"id"`
				Coordinator string `json:"coordinator_handle"`
			} `json:"run"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rb, &rc); err != nil || rc.Result.Run.ID == "" || rc.Result.Run.Coordinator == "" {
		t.Fatalf("run-current fixture: %v %+v", err, rc)
	}
}

func TestCoordIsSubagentInput(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{``, false},
		{`not json`, false},
		{`{"transcript_path":"/home/u/.claude/projects/p/1234.jsonl"}`, false},
		{`{"transcript_path":"/x/tasks/abc.output"}`, true},
		{`{"transcript_path":"/x/s/subagents/agent-1.jsonl"}`, true},
		{`{"transcript_path":"/home/u/.claude/projects/p/1234.jsonl","agent_id":"a1"}`, true},
	}
	for _, c := range cases {
		if got := coordIsSubagentInput([]byte(c.in)); got != c.want {
			t.Errorf("coordIsSubagentInput(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCoordWakeRetriesStaleWaiterExists(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.cfg.Backoff = 10 * time.Millisecond
	_ = os.WriteFile(filepath.Join(h.dir, "waiterexists"), nil, 0o644)
	body := fmt.Sprintf(`{"ok":true,"result":{"runId":"run_x","deliveryId":"dlv_late","messages":[%s],"count":1}}`,
		coordMsgJSON("msg_late", "question", "term_lane", "late question", "after a stale waiter"))
	_ = os.WriteFile(filepath.Join(h.dir, "later"), []byte(body), 0o644)
	if got := coordWake(h.cfg); got != 2 {
		t.Fatalf("exit %d, want 2 (wake after retrying waiter_exists); stderr=%q", got, h.errb.String())
	}
	if !strings.Contains(h.errb.String(), "msg_late") {
		t.Fatalf("wake block missing msg_late: %q", h.errb.String())
	}
}
