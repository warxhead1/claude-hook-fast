// Package timing records per-invocation hook durations so "hook-fast"
// stays measurable, not aspirational. Every hook invocation is another
// process spawn in the tool-call critical path; this is the ledger that
// catches the slow ones.
//
// Record appends one JSONL line {"ts","bin","cmd","ms"} to
// $HOOK_TIMINGS_DIR/<bin>.jsonl (default ~/.cache/hook-timings/).
// Cost is one stat + append (~100µs). Disable with HOOK_TIMINGS=0.
// Files rotate once at 4MB (<bin>.jsonl -> <bin>.jsonl.1).
package timing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const rotateBytes = 4 << 20

// Dir resolves the timings output directory. Empty string disables.
func Dir() string {
	if v := os.Getenv("HOOK_TIMINGS_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "hook-timings")
}

// Entry is one recorded invocation.
type Entry struct {
	TS  string  `json:"ts"`
	Bin string  `json:"bin"`
	Cmd string  `json:"cmd"`
	MS  float64 `json:"ms"`
}

// Record appends the invocation. Fire-and-forget: never errors, never
// blocks beyond a single small append.
func Record(bin, cmd string, d time.Duration) {
	if os.Getenv("HOOK_TIMINGS") == "0" || cmd == "" {
		return
	}
	dir := Dir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, bin+".jsonl")
	if st, err := os.Stat(path); err == nil && st.Size() > rotateBytes {
		_ = os.Rename(path, path+".1")
	}
	b, err := json.Marshal(Entry{
		TS:  time.Now().UTC().Format(time.RFC3339Nano),
		Bin: bin,
		Cmd: cmd,
		MS:  float64(d.Microseconds()) / 1000.0,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(b, '\n'))
}

// Load reads every *.jsonl and *.jsonl.1 in dir. Malformed lines are
// skipped — the file is append-only from many concurrent processes.
func Load(dir string) []Entry {
	var out []Entry
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	rotated, _ := filepath.Glob(filepath.Join(dir, "*.jsonl.1"))
	for _, p := range append(rotated, matches...) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		start := 0
		for i := 0; i <= len(data); i++ {
			if i == len(data) || data[i] == '\n' {
				line := data[start:i]
				start = i + 1
				if len(line) == 0 {
					continue
				}
				var e Entry
				if json.Unmarshal(line, &e) == nil && e.Cmd != "" {
					out = append(out, e)
				}
			}
		}
	}
	return out
}
