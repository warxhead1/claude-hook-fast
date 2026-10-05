package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func coordThreadMsgJSON(id, typ, from, subject, thread, created string) string {
	m := map[string]any{"id": id, "run_id": "run_x", "from_handle": from, "to_handle": "x",
		"subject": subject, "body": "", "type": typ, "thread_id": thread, "payload": nil, "created_at": created}
	if thread == "" {
		m["thread_id"] = nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (h *coordHarness) setInbox(msgs ...string) {
	body := fmt.Sprintf(`{"ok":true,"result":{"messages":[%s]}}`, strings.Join(msgs, ","))
	if err := os.WriteFile(filepath.Join(h.dir, "inbox.json"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *coordHarness) gate(stopActive bool) string {
	var out bytes.Buffer
	if rc := coordStopGate(h.cfg, &out, stopActive); rc != 0 {
		h.t.Fatalf("stop gate exit %d", rc)
	}
	return out.String()
}

// wakeWith delivers msgs through coord-wake so woken.json is recorded.
func (h *coordHarness) wakeWith(delivery string, msgs ...string) {
	h.t.Helper()
	h.enqueue(delivery, msgs...)
	if got := coordWake(h.cfg); got != 2 {
		h.t.Fatalf("wake exit %d", got)
	}
}

func TestCoordWakeRecordsWoken(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.wakeWith("d1",
		coordMsgJSON("msg_q1", "question", "term_lane", "Q?", "b"),
		coordMsgJSON("msg_d1", "worker_done", "term_lane", "done", "b"))
	w := coordLoadWoken(h.runDir())
	if len(w.Items) != 1 || w.Items[0].ID != "msg_q1" || w.Items[0].CreatedAt == "" {
		t.Fatalf("woken.json = %+v", w)
	}
}

func TestCoordStopGateBlocksUnreplied(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.wakeWith("d1", coordMsgJSON("msg_q1", "question", "term_lane", "Need GPU?", "b"))
	out := h.gate(false)
	var d struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Decision != "block" {
		t.Fatalf("expected block, got %q (%v)", out, err)
	}
	for _, want := range []string{"msg_q1", "Need GPU?", "Hero build: spinner", "orchestration reply --id msg_q1"} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("reason missing %q:\n%s", want, d.Reason)
		}
	}
}

func TestCoordStopGateBlocksOncePerSet(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.wakeWith("d1", coordMsgJSON("msg_q1", "question", "term_lane", "Q1", "b"))
	if h.gate(false) == "" {
		t.Fatal("first stop did not block")
	}
	if got := h.gate(false); got != "" {
		t.Fatalf("same set blocked twice: %q", got)
	}
	h.wakeWith("d2", coordMsgJSON("msg_q2", "escalation", "term_lane", "Q2", "b"))
	out := h.gate(false)
	if !strings.Contains(out, "msg_q1") || !strings.Contains(out, "msg_q2") {
		t.Fatalf("new set not blocked with both ids: %q", out)
	}
}

func TestCoordStopGateReplyLinkage(t *testing.T) {
	cases := []struct {
		name    string
		orig    string
		inbox   []string
		blocked bool
	}{
		{"reply-from-run-handle-null-thread", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"),
			[]string{coordThreadMsgJSON("msg_r1", "status", "run:run_x", "Re: Q", "msg_q1", "2026-10-05T00:01:00Z")}, false},
		{"reply-from-coordinator-self-threaded", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"),
			[]string{coordThreadMsgJSON("msg_q1", "question", "term_lane", "Q", "msg_q1", "2026-10-05T00:00:00Z"),
				coordThreadMsgJSON("msg_r1", "status", "term_coord", "Re: Q", "msg_q1", "2026-10-05T00:01:00Z")}, false},
		{"reply-from-another-lane-does-not-count", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"),
			[]string{coordThreadMsgJSON("msg_r1", "status", "term_other", "Re: Q", "msg_q1", "2026-10-05T00:01:00Z")}, true},
		{"unthreaded-coordinator-message-does-not-count", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"),
			[]string{coordThreadMsgJSON("msg_r1", "status", "term_coord", "hello", "", "2026-10-05T00:01:00Z")}, true},
		{"no-reply", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"), nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCoordHarness(t, "term_coord")
			h.wakeWith("d1", c.orig)
			h.setInbox(c.inbox...)
			out := h.gate(false)
			if (out != "") != c.blocked {
				t.Fatalf("blocked=%v want %v (out %q)", out != "", c.blocked, out)
			}
		})
	}
}

func TestCoordStopGateReplyAfterBlockPasses(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.wakeWith("d1", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"))
	if h.gate(false) == "" {
		t.Fatal("did not block")
	}
	h.setInbox(coordThreadMsgJSON("msg_r1", "status", "run:run_x", "Re: Q", "msg_q1", "2026-10-05T00:01:00Z"))
	if got := h.gate(false); got != "" {
		t.Fatalf("blocked after reply: %q", got)
	}
	if w := coordLoadWoken(h.runDir()); len(w.Items) != 0 {
		t.Fatalf("replied item not dropped: %+v", w)
	}
}

func TestCoordStopGateIgnoresStopHookActiveAndKillSwitch(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.wakeWith("d1", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"))
	if got := h.gate(true); got != "" {
		t.Fatalf("stop_hook_active blocked: %q", got)
	}
	t.Setenv("COORD_STOP_GATE_DISABLE", "1")
	if got := h.gate(false); got != "" {
		t.Fatalf("kill switch blocked: %q", got)
	}
}

func TestCoordStopGateFailsOpen(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.wakeWith("d1", coordMsgJSON("msg_q1", "question", "term_lane", "Q", "b"))
	if err := os.WriteFile(filepath.Join(h.dir, "inbox.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.gate(false); got != "" {
		t.Fatalf("blocked on unreadable inbox: %q", got)
	}
	h2 := newCoordHarness(t, "term_someone_else")
	h2.cfg.Handle = "term_coord"
	if got := h2.gate(false); got != "" {
		t.Fatalf("non-coordinator blocked: %q", got)
	}
}
