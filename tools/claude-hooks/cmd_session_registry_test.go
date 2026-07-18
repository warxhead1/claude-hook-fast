package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ---------- srProcDirExists / srReadProcStartTime ----------

func TestSrProcDirExists_AliveForSelf(t *testing.T) {
	if !srProcDirExists(os.Getpid()) {
		t.Fatal("expected /proc/<self> to exist")
	}
}

func TestSrProcDirExists_DeadForImplausiblePID(t *testing.T) {
	// PIDs are capped well below this on every real Linux system
	// (pid_max default 4194304); this pid can never be alive.
	if srProcDirExists(999999999) {
		t.Fatal("expected implausible pid to report dead")
	}
}

func TestSrProcDirExists_NonPositive(t *testing.T) {
	if srProcDirExists(0) || srProcDirExists(-1) {
		t.Fatal("expected non-positive pid to report dead")
	}
}

func TestSrReadProcStartTime_SelfIsStable(t *testing.T) {
	a, ok := srReadProcStartTime(os.Getpid())
	if !ok || a == "" {
		t.Fatalf("expected a starttime for self, got %q ok=%v", a, ok)
	}
	b, ok := srReadProcStartTime(os.Getpid())
	if !ok || a != b {
		t.Fatalf("expected stable starttime across reads: %q vs %q", a, b)
	}
}

func TestSrReadProcStartTime_MissingPID(t *testing.T) {
	_, ok := srReadProcStartTime(999999999)
	if ok {
		t.Fatal("expected ok=false for nonexistent pid")
	}
}

// ---------- srIsAncestorPID ----------

func TestSrIsAncestorPID_ParentIsAncestor(t *testing.T) {
	if !srIsAncestorPID(os.Getpid(), os.Getppid()) {
		t.Fatal("expected os.Getppid() to be an ancestor of self")
	}
}

func TestSrIsAncestorPID_SelfIsNotOwnAncestor(t *testing.T) {
	if srIsAncestorPID(os.Getpid(), os.Getpid()) {
		t.Fatal("a process is not its own ancestor")
	}
}

func TestSrIsAncestorPID_UnrelatedPIDIsNot(t *testing.T) {
	if srIsAncestorPID(os.Getpid(), 999999999) {
		t.Fatal("implausible pid must never register as an ancestor")
	}
}

func TestSrIsAncestorPID_NonPositiveCandidate(t *testing.T) {
	if srIsAncestorPID(os.Getpid(), 0) || srIsAncestorPID(os.Getpid(), -1) {
		t.Fatal("non-positive candidate can never be an ancestor")
	}
}

// ---------- srShouldTombstone ----------

func TestSrShouldTombstone_DeadPID(t *testing.T) {
	if !srShouldTombstone(999999999, "anything", os.Getpid()) {
		t.Fatal("dead pid must be tombstoned")
	}
}

func TestSrShouldTombstone_AliveWrongProcStart(t *testing.T) {
	// A real, currently-running pid but a procStart that can't match.
	if !srShouldTombstone(os.Getpid(), "not-the-real-starttime", 1) {
		t.Fatal("alive pid with mismatched procStart must be tombstoned")
	}
}

func TestSrShouldTombstone_AliveMatchingNotAncestor(t *testing.T) {
	real, ok := srReadProcStartTime(os.Getpid())
	if !ok {
		t.Fatal("could not read own starttime")
	}
	// hookPID=1 (init) never has os.Getpid() as an ancestor.
	if srShouldTombstone(os.Getpid(), real, 1) {
		t.Fatal("alive, procStart-matching, non-ancestor pid must NOT be tombstoned")
	}
}

func TestSrShouldTombstone_AliveMatchingIsAncestor(t *testing.T) {
	real, ok := srReadProcStartTime(os.Getppid())
	if !ok {
		t.Fatal("could not read parent starttime")
	}
	// os.Getppid() is a genuine ancestor of os.Getpid() (the "hook").
	if !srShouldTombstone(os.Getppid(), real, os.Getpid()) {
		t.Fatal("alive, procStart-matching pid that is an ancestor of the hook must be tombstoned")
	}
}

// ---------- srTombstoneEndedSession ----------

func writeRegistryFile(t *testing.T, dir, name string, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRegistryFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSrTombstoneEndedSession_TombstonesDeadMatchingSession(t *testing.T) {
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "999999999.json", map[string]any{
		"pid":             999999999,
		"sessionId":       "target-session",
		"cwd":             "/home/eric/projects/hearth",
		"procStart":       "123456",
		"version":         "2.1.212",
		"kind":            "interactive",
		"bridgeSessionId": "session_abc",
	})

	n := srTombstoneEndedSession(dir, "target-session", "other", os.Getpid(), time.Now())
	if n != 1 {
		t.Fatalf("expected 1 file tombstoned, got %d", n)
	}

	got := readRegistryFile(t, path)
	if got["status"] != "ended" {
		t.Errorf("status = %v, want ended", got["status"])
	}
	if got["endReason"] != "other" {
		t.Errorf("endReason = %v, want other", got["endReason"])
	}
	if _, ok := got["endedAt"]; !ok {
		t.Error("expected endedAt to be set")
	}
	// Unknown fields must survive untouched.
	if got["cwd"] != "/home/eric/projects/hearth" {
		t.Errorf("cwd field lost: %v", got["cwd"])
	}
	if got["bridgeSessionId"] != "session_abc" {
		t.Errorf("bridgeSessionId field lost: %v", got["bridgeSessionId"])
	}
	if got["version"] != "2.1.212" {
		t.Errorf("version field lost: %v", got["version"])
	}
}

func TestSrTombstoneEndedSession_IgnoresDifferentSessionId(t *testing.T) {
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "999999999.json", map[string]any{
		"pid":       999999999,
		"sessionId": "some-other-session",
		"procStart": "1",
	})

	n := srTombstoneEndedSession(dir, "target-session", "other", os.Getpid(), time.Now())
	if n != 0 {
		t.Fatalf("expected 0 files touched, got %d", n)
	}
	got := readRegistryFile(t, path)
	if _, ok := got["status"]; ok {
		t.Error("status must not be set on a non-matching session file")
	}
}

func TestSrTombstoneEndedSession_LeavesAliveMatchingNonAncestorUntouched(t *testing.T) {
	real, ok := srReadProcStartTime(os.Getpid())
	if !ok {
		t.Fatal("could not read own starttime")
	}
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, strconv.Itoa(os.Getpid())+".json", map[string]any{
		"pid":       os.Getpid(),
		"sessionId": "target-session",
		"procStart": real,
	})

	// hookPID=1 so os.Getpid() is never an ancestor of it.
	n := srTombstoneEndedSession(dir, "target-session", "other", 1, time.Now())
	if n != 0 {
		t.Fatalf("expected alive+matching+non-ancestor entry left alone, got %d touched", n)
	}
	got := readRegistryFile(t, path)
	if _, ok := got["status"]; ok {
		t.Error("status must not be set on a live, verifiably-unrelated process")
	}
}

func TestSrTombstoneEndedSession_NeverDeletesFile(t *testing.T) {
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "999999999.json", map[string]any{
		"pid":       999999999,
		"sessionId": "target-session",
		"procStart": "1",
	})
	srTombstoneEndedSession(dir, "target-session", "other", os.Getpid(), time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file must never be deleted: %v", err)
	}
}

func TestSrTombstoneEndedSession_EmptyDirOrSession(t *testing.T) {
	if n := srTombstoneEndedSession("", "x", "other", 1, time.Now()); n != 0 {
		t.Errorf("empty dir must yield 0, got %d", n)
	}
	if n := srTombstoneEndedSession(t.TempDir(), "", "other", 1, time.Now()); n != 0 {
		t.Errorf("empty sessionID must yield 0, got %d", n)
	}
}

func TestSrTombstoneEndedSession_SkipsNonJSONAndDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "999999999.json"), 0755); err != nil {
		t.Fatal(err)
	}
	n := srTombstoneEndedSession(dir, "target-session", "other", os.Getpid(), time.Now())
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
}

// ---------- srFindAncestorRegistryFile ----------

func TestSrFindAncestorRegistryFile_FindsImmediateParent(t *testing.T) {
	dir := t.TempDir()
	writeRegistryFile(t, dir, strconv.Itoa(os.Getppid())+".json", map[string]any{
		"pid":       os.Getppid(),
		"sessionId": "old-session",
	})
	got := srFindAncestorRegistryFile(dir, os.Getpid())
	want := filepath.Join(dir, strconv.Itoa(os.Getppid())+".json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSrFindAncestorRegistryFile_NoneFound(t *testing.T) {
	dir := t.TempDir() // no registry files at all
	got := srFindAncestorRegistryFile(dir, os.Getpid())
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSrFindAncestorRegistryFile_EmptyDir(t *testing.T) {
	if got := srFindAncestorRegistryFile("", os.Getpid()); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// ---------- srRepairStaleSessionID ----------

func TestSrRepairStaleSessionID_UpdatesMismatchedSessionId(t *testing.T) {
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "123.json", map[string]any{
		"pid":       123,
		"sessionId": "old-session",
		"cwd":       "/home/eric/projects/hearth",
		"version":   "2.1.205",
	})

	changed := srRepairStaleSessionID(path, "new-session", time.Now())
	if !changed {
		t.Fatal("expected repair to report a change")
	}
	got := readRegistryFile(t, path)
	if got["sessionId"] != "new-session" {
		t.Errorf("sessionId = %v, want new-session", got["sessionId"])
	}
	if _, ok := got["updatedAt"]; !ok {
		t.Error("expected updatedAt to be set")
	}
	// Unrelated fields preserved.
	if got["cwd"] != "/home/eric/projects/hearth" {
		t.Errorf("cwd lost: %v", got["cwd"])
	}
	if got["version"] != "2.1.205" {
		t.Errorf("version lost: %v", got["version"])
	}
}

func TestSrRepairStaleSessionID_ClearsEndedStatus(t *testing.T) {
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "123.json", map[string]any{
		"pid":       123,
		"sessionId": "old-session",
		"status":    "ended",
		"endedAt":   float64(1700000000000),
		"endReason": "other",
	})

	srRepairStaleSessionID(path, "new-session", time.Now())
	got := readRegistryFile(t, path)
	if got["status"] != "idle" {
		t.Errorf("status = %v, want idle", got["status"])
	}
	if _, ok := got["endedAt"]; ok {
		t.Error("expected endedAt to be removed")
	}
	if _, ok := got["endReason"]; ok {
		t.Error("expected endReason to be removed")
	}
}

func TestSrRepairStaleSessionID_NoOpWhenAlreadyMatching(t *testing.T) {
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "123.json", map[string]any{
		"pid":       123,
		"sessionId": "same-session",
		"updatedAt": float64(1000),
	})

	changed := srRepairStaleSessionID(path, "same-session", time.Now())
	if changed {
		t.Fatal("expected no-op when sessionId already matches")
	}
	got := readRegistryFile(t, path)
	if got["updatedAt"] != float64(1000) {
		t.Errorf("updatedAt should be untouched by a no-op, got %v", got["updatedAt"])
	}
}

func TestSrRepairStaleSessionID_NeverCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")
	if srRepairStaleSessionID(path, "new-session", time.Now()) {
		t.Fatal("expected no-op for a missing file")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("must never create the registry file")
	}
}

func TestSrRepairStaleSessionID_EmptyArgs(t *testing.T) {
	if srRepairStaleSessionID("", "x", time.Now()) {
		t.Error("empty path must be a no-op")
	}
	dir := t.TempDir()
	path := writeRegistryFile(t, dir, "1.json", map[string]any{"sessionId": "a"})
	if srRepairStaleSessionID(path, "", time.Now()) {
		t.Error("empty newSessionID must be a no-op")
	}
}

// ---------- srSessionsDir ----------

func TestSrSessionsDir_UsesHOME(t *testing.T) {
	t.Setenv("HOME", "/fake/home")
	got := srSessionsDir()
	want := "/fake/home/.claude/sessions"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSrSessionsDir_EmptyWhenNoHOME(t *testing.T) {
	t.Setenv("HOME", "")
	if got := srSessionsDir(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
