package nbuspub

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// procState returns the single-letter state field from /proc/<pid>/stat, or
// "" once the process has been fully reaped (the entry disappears).
func procState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// State is the field after the parenthesized comm, which may itself
	// contain spaces — split after the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return ""
	}
	return string(s[i+2])
}

// Regression: StartDetached must REAP its child, not just Release() the
// handle. Under the pre-fix code the exited child stayed a zombie (state Z)
// for the lifetime of any long-lived caller — the codex-hook lease-worker
// accumulated one defunct child per 30s lease refresh.
func TestStartDetachedReapsChild(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := StartDetached(cmd); err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	// Go 1.23+ Release() invalidates the handle and sets Pid to -1, which
	// would silently turn the /proc poll below into a no-op pass. A reaping
	// StartDetached must keep the handle intact for its own Wait, so a
	// non-positive pid here IS the Release()-not-Wait() bug resurfacing.
	pid := cmd.Process.Pid
	if pid <= 0 {
		t.Fatalf("pid=%d after StartDetached — handle was Release()d instead of reaped via Wait", pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		switch procState(pid) {
		case "":
			return // reaped — /proc entry gone
		case "Z":
			// still a zombie; keep polling until deadline
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d still state %q after 5s — StartDetached did not reap it", pid, procState(pid))
}

// StartDetached must surface a start failure so callers can fall back to
// synchronous delivery.
func TestStartDetachedStartError(t *testing.T) {
	cmd := exec.Command("/nonexistent-binary-for-nbuspub-test")
	if err := StartDetached(cmd); err == nil {
		t.Fatal("expected error starting nonexistent binary")
	}
}
