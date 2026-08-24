package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

const (
	leasePeriod = 30 * time.Second
	leaseGrace  = 90 * time.Second
	leaseTTL    = 6 * time.Minute
)

type leaseState struct {
	SessionID   string      `json:"session_id"`
	ParentPID   int         `json:"parent_pid"`
	ParentStart string      `json:"parent_start"`
	ExpiresAt   time.Time   `json:"expires_at"`
	Payload     hookPayload `json:"payload"`
}
type workerState struct {
	PID       int       `json:"pid"`
	Start     string    `json:"start"`
	Token     string    `json:"token"`
	UpdatedAt time.Time `json:"updated_at"`
}
type paneOwner struct {
	Binding     string `json:"binding"`
	SessionID   string `json:"session_id"`
	ParentPID   int    `json:"parent_pid"`
	ParentStart string `json:"parent_start"`
}

var leaseNow = func() time.Time { return time.Now().UTC() }
var leaseStartWorker = startLeaseWorker
var processParent = procParent
var processName = procName
var leaseParentIdentity = leaseParent

func leaseDir() string {
	if d := os.Getenv("HEARTH_CODEX_LEASE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/hearth-codex-hook-leases"
	}
	return filepath.Join(home, ".cache", "hearth-codex-hook", "leases")
}
func leaseKey(id string) string { sum := sha256.Sum256([]byte(id)); return fmt.Sprintf("%x", sum[:12]) }
func leasePaths(id string) (string, string) {
	base := filepath.Join(leaseDir(), leaseKey(id))
	return base + ".json", base + ".worker.json"
}
func procStart(pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) < 22 {
		return ""
	}
	return fields[21]
}
func procParent(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			return n
		}
	}
	return 0
}
func procName(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func codexAncestor(pid int) int {
	for i := 0; i < 32 && pid > 1; i++ {
		if strings.Contains(strings.ToLower(processName(pid)), "codex") {
			return pid
		}
		next := processParent(pid)
		if next == pid {
			break
		}
		pid = next
	}
	return 0
}
func leaseParent() (int, string) {
	pid := codexAncestor(os.Getppid())
	if pid == 0 {
		pid = os.Getppid()
	}
	return pid, procStart(pid)
}
func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func readWorker(path string) (workerState, error) {
	var w workerState
	b, err := os.ReadFile(path)
	if err != nil {
		return w, err
	}
	return w, json.Unmarshal(b, &w)
}
func workerAlive(w workerState) bool {
	return w.PID > 0 && w.Start != "" && procStart(w.PID) == w.Start
}

// ensureLease is deliberately cheap: update one small state file and only
// spawn when no fresh, starttime-validated worker owns the session.
func ensureLease(p *hookPayload) {
	if p == nil || p.SessionID == "" || isSubagent(p) {
		return
	}
	statePath, workerPath := leasePaths(p.SessionID)
	parentPID, parentStart := leaseParentIdentity()
	state := leaseState{SessionID: p.SessionID, ParentPID: parentPID, ParentStart: parentStart, ExpiresAt: leaseNow().Add(leaseTTL), Payload: *p}
	if err := writeJSONAtomic(statePath, state); err != nil {
		hooklog.Writef("codex-hook lease state id=%s err=%v", p.SessionID, err)
		return
	}
	needsStart := false
	if !withWorkerLock(workerPath, func() {
		worker, err := readWorker(workerPath)
		if err == nil && workerAlive(worker) && leaseNow().Sub(worker.UpdatedAt) < leaseGrace {
			return
		}
		_ = os.Remove(workerPath)
		needsStart = true
	}) || !needsStart {
		return
	}
	if err := leaseStartWorker(p.SessionID); err != nil {
		hooklog.Writef("codex-hook lease spawn id=%s err=%v", p.SessionID, err)
	}
}
func startLeaseWorker(id string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "lease-worker", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
func loadLease(path string) (leaseState, error) {
	var s leaseState
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
func newCodexHookLeaseWorkerCmd() *cobra.Command {
	return &cobra.Command{Use: "lease-worker <session-id>", Hidden: true, Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { runLeaseWorker(args[0]); return nil }}
}
func runLeaseWorker(id string) {
	statePath, workerPath := leasePaths(id)
	me := workerState{PID: os.Getpid(), Start: procStart(os.Getpid()), Token: uuid.NewString(), UpdatedAt: leaseNow()}
	if err := writeWorkerExclusive(workerPath, me); err != nil {
		return
	}
	defer func() {
		current, err := readWorker(workerPath)
		if err == nil && current.PID == me.PID && current.Start == me.Start {
			_ = os.Remove(workerPath)
		}
	}()
	for {
		state, err := loadLease(statePath)
		if err != nil || state.SessionID != id || leaseNow().After(state.ExpiresAt) || !parentAlive(state) {
			return
		}
		current, err := readWorker(workerPath)
		if err != nil || current.PID != me.PID || current.Start != me.Start || current.Token != me.Token {
			return
		}
		emitHook(&state.Payload, "lease_refresh", true)
		me.UpdatedAt = leaseNow()
		if !writeWorkerIfOwner(workerPath, me) {
			return
		}
		time.Sleep(leasePeriod)
	}
}
func writeWorkerExclusive(path string, w workerState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}
func parentAlive(s leaseState) bool {
	return s.ParentPID > 0 && s.ParentStart != "" && procStart(s.ParentPID) == s.ParentStart
}
func cleanupLease(p *hookPayload) bool {
	if p == nil || p.SessionID == "" {
		return false
	}
	statePath, worker := leasePaths(p.SessionID)
	state, err := loadLease(statePath)
	pid, start := leaseParentIdentity()
	if err != nil || state.ParentPID != pid || state.ParentStart != start {
		return false
	}
	_ = os.Remove(statePath)
	_ = os.Remove(worker)
	return true
}

func workerLockPath(path string) string { return path + ".lock" }
func withWorkerLock(path string, fn func()) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	// Advisory locks are released by the kernel when a hook/worker crashes.
	// Keep the inode: removing a sentinel while another process waits races a
	// new, independently locked inode into existence.
	f, err := os.OpenFile(workerLockPath(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	fn()
	return true
}
func writeWorkerIfOwner(path string, next workerState) bool {
	written := false
	_ = withWorkerLock(path, func() {
		current, err := readWorker(path)
		if err != nil || current.PID != next.PID || current.Start != next.Start || current.Token != next.Token {
			return
		}
		written = writeJSONAtomic(path, next) == nil
	})
	return written
}

func paneOwnerPath(binding string) string {
	return filepath.Join(leaseDir(), "pane-"+leaseKey(binding)+".json")
}
func loadPaneOwner(path string) (paneOwner, error) {
	var owner paneOwner
	b, err := os.ReadFile(path)
	if err != nil {
		return owner, err
	}
	return owner, json.Unmarshal(b, &owner)
}
func ownerAlive(owner paneOwner) bool {
	return owner.ParentPID > 0 && owner.ParentStart != "" && procStart(owner.ParentPID) == owner.ParentStart
}
func allowPaneEmission(p *hookPayload, action string) bool {
	if p == nil || isSubagent(p) {
		return false
	}
	_, binding := qualifiedPane()
	if binding == "" {
		return false
	}
	path := paneOwnerPath(binding)
	// Existing sessions may install this hook after SessionStart already fired.
	// Any root hook can therefore claim an absent or dead owner, while the
	// exclusive live-owner check in claimPaneOwner still blocks inherited panes.
	_ = action // retained for call-site observability and future policy.
	return claimPaneOwner(path, binding, p.SessionID)
}
func claimPaneOwner(path, binding, sessionID string) bool {
	pid, start := leaseParentIdentity()
	candidate := paneOwner{Binding: binding, SessionID: sessionID, ParentPID: pid, ParentStart: start}
	if err := writePaneOwnerExclusive(path, candidate); err == nil {
		return true
	}
	owner, err := loadPaneOwner(path)
	if err != nil {
		return false
	}
	if owner.SessionID == sessionID && ownerAlive(owner) {
		return true
	}
	if ownerAlive(owner) {
		return false
	}
	_ = os.Remove(path)
	return writePaneOwnerExclusive(path, candidate) == nil
}
func writePaneOwnerExclusive(path string, owner paneOwner) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}
func releasePaneOwner(p *hookPayload) {
	if p == nil || isSubagent(p) {
		return
	}
	_, binding := qualifiedPane()
	if binding == "" {
		return
	}
	path := paneOwnerPath(binding)
	owner, err := loadPaneOwner(path)
	pid, start := leaseParentIdentity()
	if err == nil && owner.SessionID == p.SessionID && owner.ParentPID == pid && owner.ParentStart == start {
		_ = os.Remove(path)
	}
}
