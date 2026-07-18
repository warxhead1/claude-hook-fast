// Tests for readers.go.
//
// # Purpose
//
// Table-driven coverage of fileReader.Read against fixture files written
// under t.TempDir(), proving each defensive-degrade path named in the task:
// dead-pid skipping, is_subagent pane suppression, missing-usage graceful
// zero, background-worker detection via the roster, and lingering-subagent
// staleness accounting.
//
// # Design constraints
//
//   - No real $HOME/$XDG_RUNTIME_DIR is ever touched: every readerPaths field
//     used by a case points into that case's own t.TempDir().
//   - Fixtures are written as plain files (os.WriteFile / os.Chtimes), not by
//     round-tripping through the reader's own marshal helpers, so a test
//     failure can't be masked by a matching bug on both sides.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/steward"
)

func rdtWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func rdtWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	rdtWriteFile(t, path, string(data))
}

// rdtDeadPid is a pid extremely unlikely to be alive under any sandbox/CI
// this test runs in, without depending on the function under test to find one.
const rdtDeadPid = 2100000000

func TestFileReader_Read(t *testing.T) {
	fixedNow := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cfg := steward.DefaultConfig()

	type tc struct {
		name  string
		setup func(t *testing.T, dir string) readerPaths
		check func(t *testing.T, snap steward.Snapshot)
	}

	cases := []tc{
		{
			name: "missing usage cache degrades to Present false",
			setup: func(t *testing.T, dir string) readerPaths {
				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-such-usage.json"),
					SessionsDir: filepath.Join(dir, "sessions"), // empty/absent
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if snap.FiveHour.Present || snap.SevenDay.Present {
					t.Errorf("expected both windows Present=false, got five=%v seven=%v", snap.FiveHour, snap.SevenDay)
				}
				if len(snap.Sessions) != 0 {
					t.Errorf("expected no sessions, got %d", len(snap.Sessions))
				}
			},
		},
		{
			name: "usage cache flat field names",
			setup: func(t *testing.T, dir string) readerPaths {
				usage := filepath.Join(dir, "usage.json")
				rdtWriteFile(t, usage, `{
					"five_hour": {"utilization": 42.5, "resets_at": "2026-07-18T17:00:00Z"},
					"seven_day": {"utilization": 10, "resets_at": "2026-07-24T00:00:00Z"}
				}`)
				return readerPaths{UsageCache: usage, SessionsDir: filepath.Join(dir, "sessions")}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if !snap.FiveHour.Present || snap.FiveHour.Utilization != 42.5 {
					t.Errorf("five_hour = %+v, want Present=true Utilization=42.5", snap.FiveHour)
				}
				wantReset := time.Date(2026, 7, 18, 17, 0, 0, 0, time.UTC)
				if !snap.FiveHour.ResetsAt.Equal(wantReset) {
					t.Errorf("five_hour.ResetsAt = %v, want %v", snap.FiveHour.ResetsAt, wantReset)
				}
				if !snap.SevenDay.Present || snap.SevenDay.Utilization != 10 {
					t.Errorf("seven_day = %+v, want Present=true Utilization=10", snap.SevenDay)
				}
			},
		},
		{
			name: "usage cache wrapped under usage key with used_percentage",
			setup: func(t *testing.T, dir string) readerPaths {
				usage := filepath.Join(dir, "usage.json")
				rdtWriteFile(t, usage, `{
					"usage": {
						"five_hour": {"used_percentage": 88, "resets_at": "2026-07-18T20:00:00Z"},
						"seven_day": {"used_percentage": 3}
					}
				}`)
				return readerPaths{UsageCache: usage, SessionsDir: filepath.Join(dir, "sessions")}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if !snap.FiveHour.Present || snap.FiveHour.Utilization != 88 {
					t.Errorf("five_hour = %+v, want Present=true Utilization=88", snap.FiveHour)
				}
				if !snap.SevenDay.Present || snap.SevenDay.Utilization != 3 {
					t.Errorf("seven_day = %+v, want Present=true Utilization=3", snap.SevenDay)
				}
			},
		},
		{
			name: "dead pid session file is skipped",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "999999.json"), rdSessionFile{
					Pid:       rdtDeadPid,
					SessionID: "dead-session",
					Status:    "idle",
				})
				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 0 {
					t.Errorf("expected dead-pid session skipped, got %d sessions: %+v", len(snap.Sessions), snap.Sessions)
				}
			},
		},
		{
			name: "live session with idle status computes IdleFor/CacheAge",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				statusAt := fixedNow.Add(-7 * time.Minute).UnixMilli()
				rdtWriteJSON(t, filepath.Join(sessDir, "self.json"), rdSessionFile{
					Pid:             os.Getpid(),
					SessionID:       "live-session",
					Name:            "my-session",
					Status:          "idle",
					StatusUpdatedAt: statusAt,
				})
				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				s := snap.Sessions[0]
				if s.ID != "live-session" || s.Name != "my-session" || s.Status != "idle" {
					t.Errorf("session = %+v, want ID=live-session Name=my-session Status=idle", s)
				}
				if s.IdleFor != 7*time.Minute {
					t.Errorf("IdleFor = %v, want 7m", s.IdleFor)
				}
				if s.CacheAge != s.IdleFor {
					t.Errorf("CacheAge = %v, want == IdleFor (%v)", s.CacheAge, s.IdleFor)
				}
			},
		},
		{
			name: "context file sets ContextPct and tmux PaneID",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "self.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "sess-ctx",
					Status:    "busy",
				})
				ctxDir := filepath.Join(dir, "runctx")
				pane := "%7"
				rdtWriteJSON(t, filepath.Join(ctxDir, "claude-context-sess-ctx.json"), rdContextFile{
					CurrentContext: 63,
					TmuxPane:       &pane,
				})
				return readerPaths{
					UsageCache:        filepath.Join(dir, "no-usage.json"),
					SessionsDir:       sessDir,
					ContextRuntimeDir: ctxDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				s := snap.Sessions[0]
				if s.ContextPct != 63 {
					t.Errorf("ContextPct = %d, want 63", s.ContextPct)
				}
				if s.PaneID != "tmux:%7" {
					t.Errorf("PaneID = %q, want tmux:%%7", s.PaneID)
				}
			},
		},
		{
			name: "context file zellij fallback pane and is_subagent suppression on a second session",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "sess-zellij",
					Status:    "busy",
				})
				rdtWriteJSON(t, filepath.Join(sessDir, "b.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "sess-subagent-ctx",
					Status:    "busy",
				})
				ctxDir := filepath.Join(dir, "runctx")
				zid := int64(4)
				rdtWriteJSON(t, filepath.Join(ctxDir, "claude-context-sess-zellij.json"), rdContextFile{
					CurrentContext: 20,
					ZellijPaneID:   &zid,
				})
				// This context file's is_subagent:true must never bind the pane
				// (or context pct) onto the parent session it happens to name.
				rdtWriteJSON(t, filepath.Join(ctxDir, "claude-context-sess-subagent-ctx.json"), rdContextFile{
					CurrentContext: 99,
					ZellijPaneID:   &zid,
					IsSubagent:     true,
				})
				return readerPaths{
					UsageCache:        filepath.Join(dir, "no-usage.json"),
					SessionsDir:       sessDir,
					ContextRuntimeDir: ctxDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				byID := map[string]steward.SessionState{}
				for _, s := range snap.Sessions {
					byID[s.ID] = s
				}
				zellij, ok := byID["sess-zellij"]
				if !ok {
					t.Fatalf("missing sess-zellij")
				}
				if zellij.PaneID != "zellij:4" {
					t.Errorf("zellij PaneID = %q, want zellij:4", zellij.PaneID)
				}
				subCtx, ok := byID["sess-subagent-ctx"]
				if !ok {
					t.Fatalf("missing sess-subagent-ctx")
				}
				if subCtx.PaneID != "" || subCtx.ContextPct != 0 {
					t.Errorf("is_subagent context file leaked onto parent session: %+v", subCtx)
				}
			},
		},
		{
			name: "roster match sets KindBackground and PtyAuthAvail true when socket exists",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "worker-1",
					Status:    "busy",
				})
				ptySock := filepath.Join(dir, "worker-1.sock")
				rdtWriteFile(t, ptySock, "socket-placeholder")
				roster := filepath.Join(dir, "roster.json")
				rdtWriteJSON(t, roster, rdRosterFile{Workers: map[string]rdRosterWorker{
					"w1": {SessionID: "worker-1", Pid: os.Getpid(), PtySock: ptySock, PtyAuth: "secret-token"},
				}})
				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
					RosterFile:  roster,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				s := snap.Sessions[0]
				if s.Kind != steward.KindBackground {
					t.Errorf("Kind = %v, want KindBackground", s.Kind)
				}
				if !s.PtyAuthAvail {
					t.Errorf("PtyAuthAvail = false, want true (socket exists + non-empty ptyAuth)")
				}
				if s.PtySock == "" {
					t.Errorf("PtySock empty, want the roster path")
				}
			},
		},
		{
			name: "roster match with missing socket file yields PtyAuthAvail false",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "worker-2",
					Status:    "busy",
				})
				roster := filepath.Join(dir, "roster.json")
				rdtWriteJSON(t, roster, rdRosterFile{Workers: map[string]rdRosterWorker{
					"w2": {SessionID: "worker-2", Pid: os.Getpid(), PtySock: filepath.Join(dir, "no-such.sock"), PtyAuth: "secret"},
				}})
				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
					RosterFile:  roster,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				s := snap.Sessions[0]
				if s.Kind != steward.KindBackground {
					t.Errorf("Kind = %v, want KindBackground (roster match alone is enough)", s.Kind)
				}
				if s.PtyAuthAvail {
					t.Errorf("PtyAuthAvail = true, want false (socket file absent)")
				}
			},
		},
		{
			name: "ignore marker sets Ignored true",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "ignored-sess",
					Status:    "idle",
				})
				ignoreDir := filepath.Join(dir, "ignore")
				rdtWriteFile(t, filepath.Join(ignoreDir, "ignored-sess"), "")
				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
					IgnoreDir:   ignoreDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 || !snap.Sessions[0].Ignored {
					t.Errorf("expected Ignored=true, got %+v", snap.Sessions)
				}
			},
		},
		{
			name: "last-compact file parsed into LastCompact",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "compacted-sess",
					Status:    "idle",
				})
				lastCompactDir := filepath.Join(dir, "last-compact")
				rdtWriteFile(t, filepath.Join(lastCompactDir, "compacted-sess"), "1786881600")
				return readerPaths{
					UsageCache:     filepath.Join(dir, "no-usage.json"),
					SessionsDir:    sessDir,
					LastCompactDir: lastCompactDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				want := time.Unix(1786881600, 0)
				if !snap.Sessions[0].LastCompact.Equal(want) {
					t.Errorf("LastCompact = %v, want %v", snap.Sessions[0].LastCompact, want)
				}
			},
		},
		{
			name: "lingering subagent staleness gates SubagentsActive but always appears in snap.Subagents",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "parent-sess",
					Status:    "busy",
				})
				projectsDir := filepath.Join(dir, "projects")
				subDir := filepath.Join(projectsDir, "-home-eric-projects-foo", "parent-sess", "subagents")

				recent := filepath.Join(subDir, "agent-recent.jsonl")
				rdtWriteFile(t, recent, `{}`)
				if err := os.Chtimes(recent, fixedNow.Add(-1*time.Minute), fixedNow.Add(-1*time.Minute)); err != nil {
					t.Fatalf("Chtimes: %v", err)
				}

				stale := filepath.Join(subDir, "agent-stale.jsonl")
				rdtWriteFile(t, stale, `{}`)
				rdtWriteFile(t, filepath.Join(subDir, "agent-stale.meta.json"), `{"name":"stale-worker"}`)
				if err := os.Chtimes(stale, fixedNow.Add(-30*time.Minute), fixedNow.Add(-30*time.Minute)); err != nil {
					t.Fatalf("Chtimes: %v", err)
				}

				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
					ProjectsDir: projectsDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				s := snap.Sessions[0]
				// cfg.LingeringSubagentAfter defaults to 10m: the 1m-old subagent
				// counts as active; the 30m-old one does not (it is "lingering").
				if s.SubagentsActive != 1 {
					t.Errorf("SubagentsActive = %d, want 1", s.SubagentsActive)
				}
				if len(snap.Subagents) != 2 {
					t.Fatalf("expected 2 subagent records, got %d: %+v", len(snap.Subagents), snap.Subagents)
				}
				var staleSub *steward.SubagentState
				for i := range snap.Subagents {
					if snap.Subagents[i].ID == "agent-stale" {
						staleSub = &snap.Subagents[i]
					}
				}
				if staleSub == nil {
					t.Fatalf("agent-stale subagent record not found in %+v", snap.Subagents)
				}
				if staleSub.Name != "stale-worker" {
					t.Errorf("Name = %q, want stale-worker (from .meta.json)", staleSub.Name)
				}
				if !staleSub.Lingering(cfg.LingeringSubagentAfter) {
					t.Errorf("expected agent-stale to be Lingering() past %v threshold", cfg.LingeringSubagentAfter)
				}
			},
		},
		{
			name: "completed subagent is Stopped via parent tool_result and excluded from active + lingering",
			setup: func(t *testing.T, dir string) readerPaths {
				sessDir := filepath.Join(dir, "sessions")
				rdtWriteJSON(t, filepath.Join(sessDir, "a.json"), rdSessionFile{
					Pid:       os.Getpid(),
					SessionID: "parent-sess",
					Status:    "busy",
				})
				projectsDir := filepath.Join(dir, "projects")
				slugDir := filepath.Join(projectsDir, "-home-eric-projects-foo")
				subDir := filepath.Join(slugDir, "parent-sess", "subagents")

				// A stale transcript for a subagent that DID finish: its meta
				// carries a toolUseId and the parent transcript records the
				// matching tool_result. Without the completion signal this 30m
				// stale record would be reported as lingering (the old bug).
				done := filepath.Join(subDir, "agent-done.jsonl")
				rdtWriteFile(t, done, `{}`)
				rdtWriteFile(t, filepath.Join(subDir, "agent-done.meta.json"),
					`{"name":"done-worker","toolUseId":"toolu_DONE"}`)
				if err := os.Chtimes(done, fixedNow.Add(-30*time.Minute), fixedNow.Add(-30*time.Minute)); err != nil {
					t.Fatalf("Chtimes: %v", err)
				}
				rdtWriteFile(t, filepath.Join(slugDir, "parent-sess.jsonl"),
					`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_DONE","content":"ok"}]}}`+"\n")

				return readerPaths{
					UsageCache:  filepath.Join(dir, "no-usage.json"),
					SessionsDir: sessDir,
					ProjectsDir: projectsDir,
				}
			},
			check: func(t *testing.T, snap steward.Snapshot) {
				if len(snap.Sessions) != 1 {
					t.Fatalf("expected 1 session, got %d", len(snap.Sessions))
				}
				if got := snap.Sessions[0].SubagentsActive; got != 0 {
					t.Errorf("SubagentsActive = %d, want 0 (the only subagent completed)", got)
				}
				if len(snap.Subagents) != 1 {
					t.Fatalf("expected 1 subagent record, got %d: %+v", len(snap.Subagents), snap.Subagents)
				}
				sub := snap.Subagents[0]
				if !sub.Stopped {
					t.Errorf("expected agent-done to be Stopped (parent transcript carries its tool_result)")
				}
				if sub.Lingering(cfg.LingeringSubagentAfter) {
					t.Errorf("a completed subagent must never be Lingering(), even when 30m stale")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := c.setup(t, dir)
			// Fields the case did not set must still be redirected into dir,
			// never left to default onto the real $HOME.
			if paths.RosterFile == "" {
				paths.RosterFile = filepath.Join(dir, "no-roster.json")
			}
			if paths.ProjectsDir == "" {
				paths.ProjectsDir = filepath.Join(dir, "projects")
			}
			if paths.IgnoreDir == "" {
				paths.IgnoreDir = filepath.Join(dir, "ignore")
			}
			if paths.LastCompactDir == "" {
				paths.LastCompactDir = filepath.Join(dir, "last-compact")
			}
			if paths.CompletionCache == "" {
				paths.CompletionCache = filepath.Join(dir, "completions.json")
			}
			if paths.ContextRuntimeDir == "" {
				paths.ContextRuntimeDir = filepath.Join(dir, "runctx")
			}
			if paths.ContextFallbackDir == "" {
				paths.ContextFallbackDir = filepath.Join(dir, "tmpctx")
			}

			r := newFileReader(paths)
			snap, err := r.Read(context.Background(), fixedNow, steward.LevelNormal, cfg)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if snap.Now != fixedNow {
				t.Errorf("Now = %v, want %v", snap.Now, fixedNow)
			}
			if snap.PrevLevel != steward.LevelNormal {
				t.Errorf("PrevLevel = %v, want LevelNormal", snap.PrevLevel)
			}
			c.check(t, snap)
		})
	}
}

func TestFileReader_Read_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	r := newFileReader(readerPaths{
		UsageCache:      filepath.Join(dir, "no-usage.json"),
		SessionsDir:     filepath.Join(dir, "sessions"),
		CompletionCache: filepath.Join(dir, "completions.json"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Read(ctx, time.Now(), steward.LevelNormal, steward.DefaultConfig()); err == nil {
		t.Errorf("expected error from already-canceled context")
	}
}

func TestNewFileReader_Defaults(t *testing.T) {
	t.Setenv("HOME", "/tmp/rdt-fake-home")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/rdt-fake-xdg")
	r := newFileReader(readerPaths{})
	if r.paths.UsageCache != "/tmp/claude-usage-cache.json" {
		t.Errorf("UsageCache default = %q", r.paths.UsageCache)
	}
	if r.paths.SessionsDir != "/tmp/rdt-fake-home/.claude/sessions" {
		t.Errorf("SessionsDir default = %q", r.paths.SessionsDir)
	}
	if r.paths.ContextRuntimeDir != "/tmp/rdt-fake-xdg/hearth/context" {
		t.Errorf("ContextRuntimeDir default = %q", r.paths.ContextRuntimeDir)
	}
	if r.paths.RosterFile != "/tmp/rdt-fake-home/.claude/daemon/roster.json" {
		t.Errorf("RosterFile default = %q", r.paths.RosterFile)
	}
}
