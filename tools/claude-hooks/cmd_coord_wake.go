package main

// cmd_coord_wake.go — coord-wake: async+asyncRewake waiter that turns
// lane decisions into a wake-up for the coordinator session
// (docs/coordinator-inbox.md, component A).
//
// Wired on PostToolUse (matcher *) and Notification (idle_prompt). Acts only
// when ORCA_TERMINAL_HANDLE is the bound run's coordinator; holds one flock
// per run; passes the previous wake's delivery id as --ack on the next run
// (read = ack, at-least-once); drains status/heartbeat into lanes.json;
// exits 2 with a stderr block when a question/escalation/worker_done
// arrives. Every failure path exits 0. Kill switch: COORD_WAKE_DISABLE=1.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type coordConfig struct {
	Handle     string
	OrcaBin    string
	StateRoot  string
	Slice      time.Duration // --timeout-ms per wait slice
	MinCycle   time.Duration // floor between empty cycles (no hot loop)
	WatchEvery time.Duration // ancestor-liveness poll interval
	CacheTTL   time.Duration // run-current verdict cache
	Ancestors  []int
	Stderr     io.Writer
}

var coordWakeTypes = "question,escalation,worker_done"

func coordEnvDur(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return def
}

func coordConfigFromEnv() coordConfig {
	bin := os.Getenv("COORD_ORCA_BIN")
	if bin == "" {
		bin = "orca-ide"
	}
	return coordConfig{
		Handle:     os.Getenv("ORCA_TERMINAL_HANDLE"),
		OrcaBin:    bin,
		StateRoot:  coordStateRoot(),
		Slice:      coordEnvDur("COORD_SLICE_MS", 5*time.Minute),
		MinCycle:   coordEnvDur("COORD_MIN_CYCLE_MS", 2*time.Second),
		WatchEvery: coordEnvDur("COORD_WATCH_MS", 5*time.Second),
		CacheTTL:   coordEnvDur("COORD_CACHE_TTL_MS", 2*time.Minute),
		Stderr:     os.Stderr,
	}
}

func cmdCoordWake() {
	hookExit(coordWake(coordConfigFromEnv()))
}

func coordIsActionable(t string) bool {
	return t == "question" || t == "escalation" || t == "worker_done"
}

func coordWake(cfg coordConfig) int {
	if os.Getenv("COORD_WAKE_DISABLE") == "1" || cfg.Handle == "" {
		return 0
	}
	if cfg.Ancestors == nil {
		cfg.Ancestors = coordAncestors(8)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runID, isCoord := coordResolveRun(ctx, cfg)
	if runID == "" || !isCoord {
		return 0
	}
	dir := filepath.Join(cfg.StateRoot, coordSafeName(runID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0
	}
	lock, err := os.OpenFile(filepath.Join(dir, "wake.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0
	}
	defer func() { _ = lock.Close() }()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return 0
	}

	gone := make(chan struct{})
	go func() {
		t := time.NewTicker(cfg.WatchEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !coordAllAlive(cfg.Ancestors) {
					close(gone)
					cancel()
					return
				}
			}
		}
	}()
	parentGone := func() bool {
		select {
		case <-gone:
			return true
		default:
			return !coordAllAlive(cfg.Ancestors)
		}
	}

	ackPath := filepath.Join(dir, "pending-ack")
	ack := ""
	if b, err := os.ReadFile(ackPath); err == nil {
		ack = strings.TrimSpace(string(b))
	}
	coordLog(dir, "start pid=%d handle=%s ack=%q", os.Getpid(), cfg.Handle, ack)
	ledger := coordLoadLedger(dir, runID)
	titles := map[string]string{}

	check := func(wait bool) (*coordCheckResp, error) {
		args := []string{"orchestration", "check", "--run", runID, "--json"}
		if ack != "" {
			args = append(args, "--ack", ack)
		}
		if wait {
			args = append(args, "--types", coordWakeTypes, "--wait",
				"--timeout-ms", strconv.Itoa(int(cfg.Slice/time.Millisecond)))
		}
		out, err := coordOrca(ctx, cfg.OrcaBin, args...)
		if err != nil && len(out) == 0 {
			return nil, err
		}
		var r coordCheckResp
		if jerr := json.Unmarshal(out, &r); jerr != nil {
			return nil, fmt.Errorf("bad json: %v", jerr)
		}
		if !r.OK {
			msg := ""
			if r.Error != nil {
				msg = r.Error.Code + " " + r.Error.Message
			}
			if wait && strings.Contains(strings.ToLower(msg), "timed out") {
				return &coordCheckResp{OK: true}, nil
			}
			return nil, fmt.Errorf("orca error: %s", msg)
		}
		if ack != "" {
			ack = ""
			_ = os.Remove(ackPath)
		}
		return &r, nil
	}

	for {
		if parentGone() {
			coordLog(dir, "exit parent-gone")
			return 0
		}
		cycleStart := time.Now()
		resp, err := check(false)
		if err == nil && len(resp.Result.Messages) == 0 {
			resp, err = check(true)
		}
		if err != nil {
			if parentGone() {
				coordLog(dir, "exit parent-gone")
			} else {
				coordLog(dir, "exit orca-failure: %v", err)
			}
			return 0
		}
		msgs := resp.Result.Messages
		if len(msgs) == 0 {
			if d := cfg.MinCycle - time.Since(cycleStart); d > 0 {
				time.Sleep(d)
			}
			continue
		}
		var actionable []coordMsg
		for _, m := range msgs {
			if coordIsActionable(m.Type) {
				actionable = append(actionable, m)
				ledger.recordActionable(m)
			} else {
				ledger.recordTelemetry(m)
			}
		}
		did := resp.Result.DeliveryID
		if len(actionable) > 0 {
			coordFillTitles(ctx, cfg, runID, ledger, titles)
			ledger.save(dir)
			if did != "" {
				_ = coordWriteAtomic(ackPath, []byte(did))
			}
			coordLog(dir, "wake delivery=%s actionable=%d telemetry=%d", did, len(actionable), len(msgs)-len(actionable))
			_, _ = io.WriteString(cfg.Stderr, coordRenderWake(runID, actionable, ledger))
			return 2
		}
		coordFillTitles(ctx, cfg, runID, ledger, titles)
		ledger.save(dir)
		coordLog(dir, "telemetry delivery=%s n=%d", did, len(msgs))
		if did != "" {
			ack = did
			_ = coordWriteAtomic(ackPath, []byte(did))
		}
	}
}

// coordFillTitles resolves lane titles from task-list once per process, and
// only when some lane with a task id has no title yet.
func coordFillTitles(ctx context.Context, cfg coordConfig, runID string, l *coordLedger, cache map[string]string) {
	need := false
	for _, ln := range l.Lanes {
		if ln.TaskID != "" && ln.Title == "" {
			if t, ok := cache[ln.TaskID]; ok {
				ln.Title = t
			} else {
				need = true
			}
		}
	}
	if !need {
		return
	}
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "task-list", "--run", runID, "--json")
	if err != nil {
		return
	}
	var tl struct {
		Result struct {
			Tasks []struct {
				ID    string `json:"id"`
				Title string `json:"task_title"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &tl) != nil {
		return
	}
	for _, t := range tl.Result.Tasks {
		cache[t.ID] = t.Title
	}
	for _, ln := range l.Lanes {
		if ln.TaskID != "" && ln.Title == "" {
			ln.Title = cache[ln.TaskID]
		}
	}
}

func coordRenderWake(runID string, msgs []coordMsg, l *coordLedger) string {
	var b strings.Builder
	fmt.Fprintf(&b, "COORDINATOR INBOX (run %s): %d lane message(s) need a decision. Answer each with the reply command shown, then continue.\n", runID, len(msgs))
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt < msgs[j].CreatedAt })
	for _, m := range msgs {
		name := m.From
		if ln := l.Lanes[m.From]; ln != nil && ln.Title != "" {
			name = ln.Title + " (" + m.From + ")"
		}
		fmt.Fprintf(&b, "\n--- %s | %s | %s\n", name, m.Type, m.ID)
		fmt.Fprintf(&b, "subject: %s\n", m.Subject)
		if m.Body != "" {
			fmt.Fprintf(&b, "body: %s\n", coordTrunc(m.Body, 1500))
		}
		if m.Type == "question" || m.Type == "escalation" {
			fmt.Fprintf(&b, "reply: orca-ide orchestration reply --id %s --body \"...\"\n", m.ID)
		} else {
			fmt.Fprintf(&b, "next: review the lane report, then integrate or re-dispatch (task %s)\n", l.Lanes[m.From].taskOrDash())
		}
	}
	return b.String()
}

func (ln *coordLane) taskOrDash() string {
	if ln == nil || ln.TaskID == "" {
		return "-"
	}
	return ln.TaskID
}
