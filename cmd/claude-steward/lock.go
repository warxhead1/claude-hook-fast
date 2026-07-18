// lock.go — single-instance guard for claude-steward.
//
// Only one steward may run per user, or two daemons would race on budget.json
// and double-fire compactions. We take an exclusive advisory flock on a lock
// file; a second instance fails fast rather than blocking. The lock releases
// automatically when the process exits (the fd closes), so a crashed daemon
// leaves no stale lock.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// singleInstanceLock holds the lock fd for the process lifetime.
type singleInstanceLock struct{ f *os.File }

// acquireLock takes an exclusive non-blocking flock on path. It returns an error
// if another instance already holds it.
func acquireLock(path string) (*singleInstanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // operator-owned path
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another claude-steward holds %s: %w", path, err)
	}
	// Record our pid for observability; not used for locking.
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &singleInstanceLock{f: f}, nil
}

// release drops the lock (also happens automatically on process exit).
func (l *singleInstanceLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
