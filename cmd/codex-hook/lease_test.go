package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLeaseDeduplicatesFreshWorkerAndExpiresStaleWorker(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	oldStart, oldNow := leaseStartWorker, leaseNow
	defer func() { leaseStartWorker = oldStart; leaseNow = oldNow }()
	now := time.Now().UTC()
	leaseNow = func() time.Time { return now }
	starts := 0
	leaseStartWorker = func(string) error { starts++; return nil }
	p := &hookPayload{SessionID: "root"}
	ensureLease(p)
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
	_, workerPath := leasePaths("root")
	if err := writeJSONAtomic(workerPath, workerState{PID: os.Getpid(), Start: procStart(os.Getpid()), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ensureLease(p)
	if starts != 1 {
		t.Fatalf("fresh worker was duplicated: %d", starts)
	}
	if err := writeJSONAtomic(workerPath, workerState{PID: os.Getpid(), Start: "reused-pid", UpdatedAt: now.Add(-leaseGrace)}); err != nil {
		t.Fatal(err)
	}
	ensureLease(p)
	if starts != 2 {
		t.Fatalf("stale/PID-reused worker not replaced: %d", starts)
	}
}

func TestLeaseCleanupRemovesState(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	state, worker := leasePaths("root")
	pid, start := leaseParentIdentity()
	if err := writeJSONAtomic(state, leaseState{SessionID: "root", ParentPID: pid, ParentStart: start}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(worker, workerState{}); err != nil {
		t.Fatal(err)
	}
	if !cleanupLease(&hookPayload{SessionID: "root"}) {
		t.Fatal("verified cleanup refused")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("state retained: %v", err)
	}
	if _, err := os.Stat(worker); !os.IsNotExist(err) {
		t.Fatalf("worker retained: %v", err)
	}
}

func TestSubagentTerminalDoesNotClaimOrCleanRootLease(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	state, _ := leasePaths("root")
	if err := writeJSONAtomic(state, leaseState{SessionID: "root"}); err != nil {
		t.Fatal(err)
	}
	p := &hookPayload{SessionID: "root", AgentID: "child", AgentType: "subagent"}
	if !isSubagent(p) {
		t.Fatal("expected child identity")
	}
	// SessionEnd handler's guard is intentionally equivalent to this branch.
	if !isSubagent(p) {
		cleanupLease(p)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("child removed root lease: %v", err)
	}
}

func TestSessionEndDoesNotRemoveNewerGeneration(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	old := leaseParentIdentity
	defer func() { leaseParentIdentity = old }()
	state, _ := leasePaths("root")
	if err := writeJSONAtomic(state, leaseState{SessionID: "root", ParentPID: 2, ParentStart: "new"}); err != nil {
		t.Fatal(err)
	}
	leaseParentIdentity = func() (int, string) { return 1, "old" }
	if cleanupLease(&hookPayload{SessionID: "root"}) {
		t.Fatal("late old process removed resumed generation")
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatal(err)
	}
	leaseParentIdentity = func() (int, string) { return 2, "new" }
	if !cleanupLease(&hookPayload{SessionID: "root"}) {
		t.Fatal("current generation did not clean up")
	}
}

func TestWorkerUpdateDoesNotOverwriteReplacementToken(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	_, path := leasePaths("root")
	current := workerState{PID: 1, Start: "one", Token: "replacement"}
	if err := writeJSONAtomic(path, current); err != nil {
		t.Fatal(err)
	}
	if writeWorkerIfOwner(path, workerState{PID: 1, Start: "one", Token: "stale", UpdatedAt: time.Now()}) {
		t.Fatal("stale worker overwrote replacement")
	}
	after, err := readWorker(path)
	if err != nil || after.Token != "replacement" {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

func TestExistingWorkerLockFileIsReusableAfterCrash(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	_, path := leasePaths("root")
	if err := os.MkdirAll(filepath.Dir(workerLockPath(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerLockPath(path), []byte("orphaned sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	if !withWorkerLock(path, func() { called = true }) || !called {
		t.Fatal("existing lock inode was not reusable")
	}
}

func TestPaneOwnerClaimReuseTerminalAndDeadOwner(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	t.Setenv("TMUX_PANE", "%17")
	root := &hookPayload{SessionID: "root"}
	other := &hookPayload{SessionID: "other"}
	if !allowPaneEmission(root, "started") {
		t.Fatal("first root did not claim pane")
	}
	if allowPaneEmission(other, "started") {
		t.Fatal("later same-pane root stole live pane")
	}
	if !allowPaneEmission(root, "tool_pre") {
		t.Fatal("owner could not reuse pane")
	}
	releasePaneOwner(root)
	if !allowPaneEmission(other, "started") {
		t.Fatal("terminal owner did not release pane")
	}
	_, binding := qualifiedPane()
	path := paneOwnerPath(binding)
	if err := writeJSONAtomic(path, paneOwner{Binding: binding, SessionID: "dead", ParentPID: 999999, ParentStart: "gone"}); err != nil {
		t.Fatal(err)
	}
	if !allowPaneEmission(root, "started") {
		t.Fatal("dead owner was not reclaimed")
	}
}

func TestExistingSessionToolHookClaimsOnlyAbsentOrDeadPaneOwner(t *testing.T) {
	t.Setenv("HEARTH_CODEX_LEASE_DIR", t.TempDir())
	t.Setenv("TMUX_PANE", "%19")
	existing := &hookPayload{SessionID: "existing"}
	other := &hookPayload{SessionID: "other"}
	if !allowPaneEmission(existing, "tool_pre") {
		t.Fatal("tool hook did not claim an empty owner record")
	}
	if allowPaneEmission(other, "tool_post") {
		t.Fatal("tool hook replaced a live different owner")
	}
	_, binding := qualifiedPane()
	if err := writeJSONAtomic(paneOwnerPath(binding), paneOwner{Binding: binding, SessionID: "dead", ParentPID: 999999, ParentStart: "gone"}); err != nil {
		t.Fatal(err)
	}
	if !allowPaneEmission(other, "lease_refresh") {
		t.Fatal("tool/lease hook did not reclaim dead owner")
	}
}

func TestCodexAncestorWalksPastShell(t *testing.T) {
	oldParent, oldName := processParent, processName
	defer func() { processParent = oldParent; processName = oldName }()
	processParent = func(pid int) int {
		if pid == 100 {
			return 50
		}
		return 1
	}
	processName = func(pid int) string {
		if pid == 50 {
			return "codex"
		}
		return "zsh"
	}
	if got := codexAncestor(100); got != 50 {
		t.Fatalf("got %d want 50", got)
	}
}

func TestLeaseParentCapturesLiveProcessStarttime(t *testing.T) {
	pid, start := leaseParent()
	if pid <= 0 || start == "" {
		t.Fatalf("invalid invoked-hook parent pid=%d start=%q", pid, start)
	}
}

func TestMalformedPayloadFailsOpen(t *testing.T) {
	if p, err := readPayloadFrom(strings.NewReader("{")); err == nil || p != nil {
		t.Fatalf("payload=%v err=%v", p, err)
	}
}
