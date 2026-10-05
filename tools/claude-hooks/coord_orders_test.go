package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const coordTestOrders = `
integration_branch = "main"

[[order]]
id = "gpu-window"
match = '(?i)(gpu|cuda|vram|solo|ue).{0,40}(window|slot|lease|hold)'
reply = "Use bbslot."
auto = true

[[order]]
id = "naming"
match = '(?i)which name'
types = ["question", "escalation"]
reply = "Use snake_case."
auto = false

[[order]]
id = "broken"
match = '([unclosed'
reply = "never"
auto = true
`

func (h *coordHarness) withOrders(body string) string {
	repo := t0Dir(h.t)
	if err := os.MkdirAll(filepath.Join(repo, ".orca"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".orca", "standing-orders.toml"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.cfg.RepoDir = repo
	return repo
}

func t0Dir(t *testing.T) string { return t.TempDir() }

func (h *coordHarness) replyCalls() []string {
	var out []string
	for _, c := range h.calls() {
		if strings.HasPrefix(c, "orchestration reply") {
			out = append(out, c)
		}
	}
	return out
}

func TestCoordOrdersLoadAndMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "standing-orders.toml"), []byte(coordTestOrders), 0o644); err != nil {
		t.Fatal(err)
	}
	o := coordLoadOrders("", dir)
	if o.IntegrationBranch != "main" || len(o.Orders) != 2 {
		t.Fatalf("loaded %+v (bad regexp order must be dropped)", o)
	}
	cases := []struct {
		m    coordMsg
		want string
	}{
		{coordMsg{Type: "question", Subject: "May I have a GPU window?"}, "gpu-window"},
		{coordMsg{Type: "question", Subject: "x", Body: "need the solo lease for 10 min"}, "gpu-window"},
		{coordMsg{Type: "escalation", Subject: "GPU window?"}, ""},
		{coordMsg{Type: "escalation", Subject: "which name?"}, "naming"},
		{coordMsg{Type: "question", Subject: "unrelated"}, ""},
	}
	for _, c := range cases {
		got := ""
		if od := o.find(c.m); od != nil {
			got = od.ID
		}
		if got != c.want {
			t.Errorf("%+v matched %q want %q", c.m, got, c.want)
		}
	}
}

func TestCoordOrdersRepoThenStateFallback(t *testing.T) {
	repo, state := t.TempDir(), t.TempDir()
	if o := coordLoadOrders(repo, state); len(o.Orders) != 0 {
		t.Fatalf("expected empty, got %+v", o)
	}
	_ = os.WriteFile(filepath.Join(state, "standing-orders.toml"), []byte(coordTestOrders), 0o644)
	if o := coordLoadOrders(repo, state); len(o.Orders) != 2 {
		t.Fatalf("state fallback not used: %+v", o)
	}
	_ = os.MkdirAll(filepath.Join(repo, ".orca"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, ".orca", "standing-orders.toml"), []byte("integration_branch = \"dev\"\n"), 0o644)
	if o := coordLoadOrders(repo, state); o.IntegrationBranch != "dev" {
		t.Fatalf("repo file should win: %+v", o)
	}
}

func TestCoordExampleOrdersParse(t *testing.T) {
	b, err := os.ReadFile("../../docs/standing-orders.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	_ = os.WriteFile(filepath.Join(state, "standing-orders.toml"), b, 0o644)
	o := coordLoadOrders("", state)
	if len(o.Orders) != 1 || !o.Orders[0].Auto || o.find(coordMsg{Type: "question", Subject: "need a GPU window"}) == nil {
		t.Fatalf("example file did not load/match: %+v", o)
	}
}

func TestCoordWakeAutoOrderSuppressesWake(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.withOrders(coordTestOrders)
	h.enqueue("d1", coordMsgJSON("msg_gpu", "question", "term_lane", "GPU window please", "b"))
	h.enqueue("d2", coordMsgJSON("msg_real", "question", "term_lane", "Real decision", "b"))
	if got := coordWake(h.cfg); got != 2 {
		t.Fatalf("exit %d", got)
	}
	out := h.errb.String()
	if strings.Contains(out, "msg_gpu") || !strings.Contains(out, "msg_real") {
		t.Fatalf("wake block wrong:\n%s", out)
	}
	rc := h.replyCalls()
	if len(rc) != 1 || !strings.Contains(rc[0], "--id msg_gpu") || !strings.Contains(rc[0], "--body Use bbslot.") {
		t.Fatalf("reply calls: %v", rc)
	}
	acked := false
	for _, c := range h.calls() {
		if strings.Contains(c, " check ") && strings.Contains(c, "--ack d1") {
			acked = true
		}
	}
	if !acked {
		t.Fatalf("auto-answered batch not acked: %v", h.calls())
	}
	l := coordLoadLedger(h.runDir(), "run_x")
	if len(l.Auto) != 1 || l.Auto[0].MessageID != "msg_gpu" || l.Auto[0].Order != "gpu-window" {
		t.Fatalf("ledger: %+v", l.Auto)
	}
	lg, _ := os.ReadFile(filepath.Join(h.runDir(), "wake.log"))
	if !strings.Contains(string(lg), "auto-reply id=msg_gpu") {
		t.Fatalf("wake.log missing auto-reply:\n%s", lg)
	}
	if w := coordLoadWoken(h.runDir()); len(w.Items) != 1 || w.Items[0].ID != "msg_real" {
		t.Fatalf("auto-answered message recorded as woken: %+v", w)
	}
}

func TestCoordWakeSuggestOrderAnnotates(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.withOrders(coordTestOrders)
	h.wakeWith("d1", coordMsgJSON("msg_n", "question", "term_lane", "Which name for the file?", "b"))
	if !strings.Contains(h.errb.String(), "standing order naming suggests: Use snake_case.") {
		t.Fatalf("no suggestion:\n%s", h.errb.String())
	}
	if len(h.replyCalls()) != 0 {
		t.Fatalf("suggest-only order replied: %v", h.replyCalls())
	}
}

func TestCoordWakeFailingAutoReplyStillWakes(t *testing.T) {
	h := newCoordHarness(t, "term_coord")
	h.withOrders(coordTestOrders)
	_ = os.WriteFile(filepath.Join(h.dir, "replyfail"), nil, 0o644)
	h.wakeWith("d1", coordMsgJSON("msg_gpu", "question", "term_lane", "GPU window please", "b"))
	out := h.errb.String()
	if !strings.Contains(out, "msg_gpu") || !strings.Contains(out, "auto-reply failed") || !strings.Contains(out, "Use bbslot.") {
		t.Fatalf("failed auto reply did not fall back to a normal wake:\n%s", out)
	}
	if l := coordLoadLedger(h.runDir(), "run_x"); len(l.Auto) != 0 {
		t.Fatalf("failed reply recorded: %+v", l.Auto)
	}
}
