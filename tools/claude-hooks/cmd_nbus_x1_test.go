package main

import (
	"testing"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

// X1 (design R6): agent.session.v1 turn_end/started gains a bare pane_id the
// hearth pane arbiter keys on — raw tmux "%N" or qualified "zellij:sess:pane"
// — and NEVER context_pct.

func TestPaneIDForArbiter(t *testing.T) {
	t.Run("tmux pane is raw %N (unprefixed)", func(t *testing.T) {
		t.Setenv("ZELLIJ_SESSION_NAME", "")
		t.Setenv("ZELLIJ_PANE_ID", "")
		t.Setenv("TMUX_PANE", "%47")
		if got := paneIDForArbiter(); got != "%47" {
			t.Fatalf("tmux: got %q want %q", got, "%47")
		}
	})
	t.Run("zellij qualified form wins over tmux", func(t *testing.T) {
		t.Setenv("ZELLIJ_SESSION_NAME", "sess")
		t.Setenv("ZELLIJ_PANE_ID", "3")
		t.Setenv("TMUX_PANE", "%47")
		if got := paneIDForArbiter(); got != "zellij:sess:3" {
			t.Fatalf("zellij: got %q want %q", got, "zellij:sess:3")
		}
	})
	t.Run("orca terminal when no multiplexer", func(t *testing.T) {
		t.Setenv("ZELLIJ_SESSION_NAME", "")
		t.Setenv("ZELLIJ_PANE_ID", "")
		t.Setenv("TMUX_PANE", "")
		t.Setenv("ORCA_TERMINAL_HANDLE", "term_abc")
		if got := paneIDForArbiter(); got != "orca:term_abc" {
			t.Fatalf("orca: got %q want %q", got, "orca:term_abc")
		}
	})
	t.Run("tmux wins over orca", func(t *testing.T) {
		t.Setenv("ZELLIJ_SESSION_NAME", "")
		t.Setenv("ZELLIJ_PANE_ID", "")
		t.Setenv("TMUX_PANE", "%9")
		t.Setenv("ORCA_TERMINAL_HANDLE", "term_abc")
		if got := paneIDForArbiter(); got != "%9" {
			t.Fatalf("tmux-over-orca: got %q", got)
		}
	})
	t.Run("neither multiplexer present", func(t *testing.T) {
		t.Setenv("ZELLIJ_SESSION_NAME", "")
		t.Setenv("ZELLIJ_PANE_ID", "")
		t.Setenv("TMUX_PANE", "")
		t.Setenv("ORCA_TERMINAL_HANDLE", "")
		if got := paneIDForArbiter(); got != "" {
			t.Fatalf("none: got %q want empty", got)
		}
	})
}

func TestSessionLifecyclePaneID_SubagentGuard(t *testing.T) {
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("ZELLIJ_PANE_ID", "")
	t.Setenv("TMUX_PANE", "%47")
	// Isolate from the real EffectiveCWD fallback chain: the HookInput
	// literals below leave CWD unset, so IsSubagent falls back to
	// os.Getwd() when CLAUDE_PROJECT_DIR is also unset — which is a
	// data2 worktree path whenever this test itself runs inside one.
	// Pin the data2 root to an empty tempdir so that fallback can never
	// match here.
	t.Setenv("HEARTH_HOOK_DATA2_WORKTREES_ROOT", t.TempDir())

	// Top-level session (no ParentToolUseID) binds the pane.
	top := event.HookInput{SessionID: "s1"}
	if got := sessionLifecyclePaneID(top); got != "%47" {
		t.Fatalf("top-level: got %q want %q", got, "%47")
	}

	// Subagent (ParentToolUseID set) inherits the parent's TMUX_PANE env, so
	// binding it would collide the subagent onto the parent's pane. Guard must
	// suppress pane_id here.
	sub := event.HookInput{SessionID: "s1", ParentToolUseID: "toolu_parent"}
	if got := sessionLifecyclePaneID(sub); got != "" {
		t.Fatalf("subagent: got %q want empty (inheritance guard)", got)
	}
}

// R6: the lifecycle envelope must carry transcript_path (X1) and must NEVER
// carry context_pct (obsoleted by hearth's server-side token computation).
func TestLifecycleEnvelope_HasTranscriptNoContextPct(t *testing.T) {
	in := event.HookInput{SessionID: "s1", TranscriptPath: "/tmp/x.jsonl"}
	env, _ := baseEnvelope(in, "turn_end", in.AgentKind())
	if _, ok := env["context_pct"]; ok {
		t.Fatal("R6 violation: lifecycle envelope must not carry context_pct")
	}
	if tp, _ := env["transcript_path"].(string); tp != "/tmp/x.jsonl" {
		t.Fatalf("X1: transcript_path missing/wrong: got %q", tp)
	}
}
