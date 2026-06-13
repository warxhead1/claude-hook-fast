// Package hooklog provides a fire-and-forget append-only log writer
// for host hook diagnostics.
//
// Hooks must not crash, must not print to stderr (the runtime captures
// it and surfaces it as a hook failure), and must not block on disk
// I/O. The log file is opened, written, closed on each call so a
// concurrent hook cannot corrupt it (O_APPEND is atomic for small
// writes on POSIX).
//
// Default path: $HOME/.claude/hooks/log/hearth-loom-host-hook.log.
// Overridable via $HEARTH_LOOM_HOST_HOOK_LOG.
package hooklog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	envOverride = "HEARTH_LOOM_HOST_HOOK_LOG"
	defaultName = "hearth-loom-host-hook.log"
)

var (
	pathOnce sync.Once
	logPath  string
)

func resolvePath() string {
	pathOnce.Do(func() {
		if v := os.Getenv(envOverride); v != "" {
			logPath = v
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			logPath = "/tmp/" + defaultName
			return
		}
		dir := filepath.Join(home, ".claude", "hooks", "log")
		_ = os.MkdirAll(dir, 0o755) // best effort
		logPath = filepath.Join(dir, defaultName)
	})
	return logPath
}

// Writef appends a formatted line, prefixed with an RFC3339 timestamp.
// Never panics, never returns an error.
func Writef(format string, args ...any) {
	line := fmt.Sprintf("%s "+format+"\n",
		append([]any{time.Now().UTC().Format(time.RFC3339Nano)}, args...)...)
	f, err := os.OpenFile(resolvePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(line)
}
