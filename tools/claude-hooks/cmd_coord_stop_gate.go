package main

// cmd_coord_stop_gate.go — coord-stop-gate: synchronous Stop hook that stops a
// coordinator turn from ending on an unanswered lane question
// (docs/coordinator-inbox.md, component B). coord-wake records every
// question/escalation it delivers in <state>/<run>/woken.json; at Stop the gate
// drops the ones with a coordinator reply and blocks once per distinct set of
// the rest. Fails open everywhere. Kill switch: COORD_STOP_GATE_DISABLE=1.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type coordWokenItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	From      string `json:"from"`
	Title     string `json:"title,omitempty"`
	Subject   string `json:"subject"`
	ThreadID  string `json:"thread_id,omitempty"`
	CreatedAt string `json:"created_at"`
}

type coordWokenFile struct {
	Items []coordWokenItem `json:"items"`
}

const coordWokenMax = 200

func coordLoadWoken(dir string) coordWokenFile {
	var w coordWokenFile
	if b, err := os.ReadFile(filepath.Join(dir, "woken.json")); err == nil {
		_ = json.Unmarshal(b, &w)
	}
	return w
}

func (w coordWokenFile) save(dir string) {
	if len(w.Items) > coordWokenMax {
		w.Items = w.Items[len(w.Items)-coordWokenMax:]
	}
	if b, err := json.MarshalIndent(w, "", "  "); err == nil {
		_ = coordWriteAtomic(filepath.Join(dir, "woken.json"), b)
	}
}

// coordRecordWoken appends delivered question/escalation messages (deduped by id).
func coordRecordWoken(dir string, msgs []coordMsg, l *coordLedger) {
	w := coordLoadWoken(dir)
	seen := map[string]bool{}
	for _, it := range w.Items {
		seen[it.ID] = true
	}
	changed := false
	for _, m := range msgs {
		if (m.Type != "question" && m.Type != "escalation") || seen[m.ID] {
			continue
		}
		title := ""
		if ln := l.Lanes[m.From]; ln != nil {
			title = ln.Title
		}
		thread := ""
		if m.ThreadID != nil {
			thread = *m.ThreadID
		}
		w.Items = append(w.Items, coordWokenItem{ID: m.ID, Type: m.Type, From: m.From,
			Title: title, Subject: m.Subject, ThreadID: thread, CreatedAt: m.CreatedAt})
		changed = true
	}
	if changed {
		w.save(dir)
	}
}

func cmdCoordStopGate() {
	data, _ := io.ReadAll(os.Stdin)
	if coordIsSubagentInput(data) {
		hookExit(0)
	}
	var in struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	_ = json.Unmarshal(data, &in)
	hookExit(coordStopGate(coordConfigFromEnv(), os.Stdout, in.StopHookActive))
}

func coordStopGate(cfg coordConfig, out io.Writer, stopHookActive bool) int {
	if os.Getenv("COORD_STOP_GATE_DISABLE") == "1" || cfg.Handle == "" || stopHookActive {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runID, isCoord := coordResolveRun(ctx, cfg)
	if runID == "" || !isCoord {
		return 0
	}
	dir := filepath.Join(cfg.StateRoot, coordSafeName(runID))
	reasons := []string{}
	if r := coordWokenBlock(ctx, cfg, runID, dir); r != "" {
		reasons = append(reasons, r)
	}
	if r := coordReapBlock(cfg, runID, dir); r != "" {
		reasons = append(reasons, r)
	}
	if len(reasons) == 0 {
		return 0
	}
	if b, err := json.Marshal(map[string]string{"decision": "block", "reason": strings.Join(reasons, "\n")}); err == nil {
		_, _ = fmt.Fprintln(out, string(b))
	}
	return 0
}

// coordReapBlock runs coord-reap (hard 20 s budget, fails open) and returns a
// block reason for a set of festering lanes not yet blocked on, or "".
func coordReapBlock(cfg coordConfig, runID, dir string) (reason string) {
	defer func() {
		if r := recover(); r != nil {
			coordLog(dir, "reap fail-open: panic %v", r)
			reason = ""
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), coordReapBudget)
	defer cancel()
	res := coordReap(ctx, cfg, runID, dir, "", true)
	items := res.Festering
	if res.Mode == "suggest" {
		items = append(append([]coordReapItem{}, res.Reapable...), items...)
	}
	if len(items) == 0 {
		return ""
	}
	hash := coordReapSetHash(items)
	p := filepath.Join(dir, "reap-blocked")
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == hash {
		return ""
	}
	_ = coordWriteAtomic(p, []byte(hash))
	coordLog(dir, "reap block set=%s", hash)
	return coordReapBlockReason(items, time.Now())
}

// coordWokenBlock returns the block reason for unanswered lane questions not
// yet blocked on, or "". Fails open.
func coordWokenBlock(ctx context.Context, cfg coordConfig, runID, dir string) string {
	woken := coordLoadWoken(dir)
	if len(woken.Items) == 0 {
		return ""
	}
	replied, err := coordRepliedIDs(ctx, cfg, runID)
	if err != nil {
		coordLog(dir, "stop-gate fail-open: %v", err)
		return ""
	}
	var open []coordWokenItem
	for _, it := range woken.Items {
		if !coordIsReplied(replied, it) {
			open = append(open, it)
		}
	}
	if len(open) != len(woken.Items) {
		coordWokenFile{Items: open}.save(dir)
	}
	if len(open) == 0 {
		return ""
	}
	ids := make([]string, len(open))
	for i, it := range open {
		ids[i] = it.ID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	hash := hex.EncodeToString(sum[:8])
	blockedPath := filepath.Join(dir, "stop-blocked")
	if b, err := os.ReadFile(blockedPath); err == nil && strings.TrimSpace(string(b)) == hash {
		return ""
	}
	_ = coordWriteAtomic(blockedPath, []byte(hash))
	coordLog(dir, "stop-gate block ids=%s", strings.Join(ids, ","))
	return coordStopReason(open)
}

func coordStopReason(open []coordWokenItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d lane message(s) delivered this session have no coordinator reply. A lane is blocked on each. Answer them before ending the turn:\n", len(open))
	for _, it := range open {
		lane := it.Title
		if lane == "" {
			lane = it.From
		}
		fmt.Fprintf(&b, "- %s | %s | %s | %s\n  reply: orca-ide orchestration reply --id %s --body \"...\"\n",
			lane, it.Type, it.ID, coordTrunc(it.Subject, 160), it.ID)
	}
	return b.String()
}

// coordRepliedIDs returns the ids that have a coordinator reply in the live
// inbox. Verified linkage (2026-10-05 inbox): a reply carries thread_id equal
// to the original message id, whether the original was self-threaded
// (thread_id == its own id) or had a null thread_id, and is sent from either
// the coordinator terminal handle or "run:<run_id>".
func coordRepliedIDs(ctx context.Context, cfg coordConfig, runID string) (map[string]string, error) {
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "inbox", "--limit", "1000", "--json")
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var in struct {
		OK     bool `json:"ok"`
		Result struct {
			Messages []coordMsg `json:"messages"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &in) != nil || !in.OK {
		return nil, fmt.Errorf("inbox unreadable")
	}
	replied := map[string]string{}
	for _, m := range in.Result.Messages {
		if m.ThreadID == nil || *m.ThreadID == "" || *m.ThreadID == m.ID {
			continue
		}
		if m.From != cfg.Handle && m.From != "run:"+runID {
			continue
		}
		if m.CreatedAt >= replied[*m.ThreadID] {
			replied[*m.ThreadID] = m.CreatedAt
		}
	}
	return replied, nil
}

// coordIsReplied: a reply on the message's own id, or a reply on the thread
// the message belongs to sent at or after the message was created.
func coordIsReplied(replied map[string]string, it coordWokenItem) bool {
	if _, ok := replied[it.ID]; ok {
		return true
	}
	if it.ThreadID != "" && it.ThreadID != it.ID {
		if at, ok := replied[it.ThreadID]; ok && at >= it.CreatedAt {
			return true
		}
	}
	return false
}
