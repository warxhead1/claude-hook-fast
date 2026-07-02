// cmd_kb_prime_siblings.go — SessionStart hook: own-project `kb prime`
// brief plus a bounded "sibling activity" section summarizing what OTHER
// live Claude Code sessions (different canonical projects) have been up
// to recently in the vault.
//
// Replaces the bare shell SessionStart entry:
//
//	timeout 2s /home/eric/.local/bin/kb prime --limit 3 2>/dev/null || true
//
// with a Go subcommand so the sibling-awareness logic (registry scan,
// per-project kb shell-outs, bounded formatting) lives in one testable
// place instead of a shell one-liner.
//
// Registry: ~/.claude/sessions/<pid>.json, one file per live/recent
// Claude Code process, written by the session tracker. Fields consumed
// here: cwd (→ project.Resolve), updatedAt (epoch millis, freshness
// gate). Unknown/extra fields are ignored so this stays forward-
// compatible with registry schema growth.
//
// Output: `{"hookSpecificOutput":{"hookEventName":"SessionStart",
// "additionalContext": "<own kb prime output>\n\n## Sibling Activity\n..."}}`
// — additionalContext (not systemMessage) is the field Claude Code
// actually folds into session context for SessionStart hooks; see
// cmd_brain_register.go for the established precedent in this repo and
// the official session-start.sh hook examples shipped with Claude Code
// plugins (explanatory-output-style, learning-output-style), which both
// use hookSpecificOutput.additionalContext. systemMessage is a
// UI-surfaced notice only — it is not delivered to the model.
//
// FAF discipline: every failure path swallows the error and returns
// silently (no stdout, exit 0). A broken sibling lookup must never
// break session start. Bounded by kpsOverallTimeout; sibling kb
// shell-outs run concurrently so wall-clock stays close to a single
// call's latency rather than the sum of all calls.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/project"
)

const (
	// kpsSiblingFreshness bounds how recently a sibling session's registry
	// file must have been touched to count as "live" — stale entries from
	// hours-old dead processes shouldn't surface as current activity.
	kpsSiblingFreshness = 30 * time.Minute

	// kpsMaxSiblings caps how many distinct sibling projects get a kb
	// shell-out — this is injected into every session's opening context,
	// so it must not scale with however many panes happen to be open.
	kpsMaxSiblings = 3

	// kpsSiblingSectionCapBytes bounds the total size of the rendered
	// "Sibling Activity" section (post-truncation), independent of the
	// per-title truncation below — belt and suspenders against context bloat.
	kpsSiblingSectionCapBytes = 500

	// kpsTitleCapRunes bounds an individual sibling headline before it's
	// even assembled into the section, so one long title can't eat the
	// whole section budget.
	kpsTitleCapRunes = 100

	// kpsPerCallTimeout bounds a single kb shell-out (own-prime or one
	// sibling-prime call). Empirically `kb prime --json` runs ~0.6-0.8s
	// warm; this leaves headroom without letting one slow project stall
	// the whole hook.
	kpsPerCallTimeout = 3 * time.Second

	// kpsOverallTimeout bounds the whole subcommand — own-prime and all
	// sibling calls run concurrently, so this is close to the true wall
	// clock budget, not a sum over calls.
	kpsOverallTimeout = 5 * time.Second
)

// kpsSessionRecord is the subset of ~/.claude/sessions/<pid>.json fields
// this hook needs. Extra registry fields (status, kind, name, ...) are
// ignored so this stays forward-compatible with registry schema growth.
type kpsSessionRecord struct {
	CWD       string `json:"cwd"`
	UpdatedAt int64  `json:"updatedAt"` // epoch millis
}

// kpsPrimeEntry is the shape of one entry inside kb prime --json's
// recent/active_plans/gaps arrays. Only Title is consumed here.
type kpsPrimeEntry struct {
	Title string `json:"title"`
}

// kpsPrimeResult is the subset of `kb prime --json` output this hook
// consumes to pick a single best headline per sibling project.
type kpsPrimeResult struct {
	Recent      []kpsPrimeEntry `json:"recent"`
	ActivePlans []kpsPrimeEntry `json:"active_plans"`
	Gaps        []kpsPrimeEntry `json:"gaps"`
	PendingVets []string        `json:"pending_vets"`
}

// headline picks the single best "what's new" line for a sibling
// project brief, falling back down the specificity ladder when the most
// recent section is empty. Returns "" when the project has nothing to
// report — callers should omit rather than print an empty line.
func (r kpsPrimeResult) headline() string {
	if len(r.Recent) > 0 && r.Recent[0].Title != "" {
		return r.Recent[0].Title
	}
	if len(r.ActivePlans) > 0 && r.ActivePlans[0].Title != "" {
		return r.ActivePlans[0].Title
	}
	if len(r.Gaps) > 0 && r.Gaps[0].Title != "" {
		return r.Gaps[0].Title
	}
	if len(r.PendingVets) > 0 && r.PendingVets[0] != "" {
		return r.PendingVets[0]
	}
	return ""
}

// resolveKbBinary mirrors resolveKbHarvest's resolution order (see
// cmd_nbus.go): prefer $HOME/.local/bin/kb, fall back to PATH. Returns
// "" when neither is found.
func resolveKbBinary() string {
	home := os.Getenv("HOME")
	if home != "" {
		candidate := filepath.Join(home, ".local", "bin", "kb")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if bin, err := osexec.LookPath("kb"); err == nil { //nolint:gosec
		return bin
	}
	return ""
}

// kpsTruncateRunes bounds s to max runes, appending "…" when truncated.
// Rune-safe (mirrors msScan's excerpt truncation in cmd_memory_staleness.go).
func kpsTruncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

// kpsSiblingProjects scans ~/.claude/sessions/*.json for other live
// sessions (updated within kpsSiblingFreshness) whose cwd resolves to a
// canonical project different from ownProject. Returns up to
// kpsMaxSiblings distinct canonical project names, most-recently-updated
// first. Every error is swallowed — an unreadable or malformed registry
// entry is simply skipped, never fatal.
func kpsSiblingProjects(ownProject string) []string {
	home := os.Getenv("HOME")
	if home == "" {
		return nil
	}
	dir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	cutoff := time.Now().Add(-kpsSiblingFreshness).UnixMilli()
	best := make(map[string]int64) // canonical project -> most recent updatedAt seen

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec kpsSessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.CWD == "" || rec.UpdatedAt < cutoff {
			continue
		}
		can, _ := project.Resolve(rec.CWD)
		if can == "" || can == "unknown" || can == ownProject {
			continue
		}
		if ts := best[can]; rec.UpdatedAt > ts {
			best[can] = rec.UpdatedAt
		}
	}

	type cand struct {
		project   string
		updatedAt int64
	}
	cands := make([]cand, 0, len(best))
	for p, ts := range best {
		cands = append(cands, cand{p, ts})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].updatedAt > cands[j].updatedAt })

	out := make([]string, 0, kpsMaxSiblings)
	for _, c := range cands {
		if len(out) >= kpsMaxSiblings {
			break
		}
		out = append(out, c.project)
	}
	return out
}

// kpsRunKbPrimeJSON shells out `kb prime --project <p> --limit 1 --json`
// and returns the parsed result. Bounded by ctx; all errors (spawn
// failure, non-zero exit, malformed JSON) yield a zero result.
func kpsRunKbPrimeJSON(ctx context.Context, binary, proj string) kpsPrimeResult {
	cmd := osexec.CommandContext(ctx, binary, "prime", "--project", proj, "--limit", "1", "--json") //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return kpsPrimeResult{}
	}
	var res kpsPrimeResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		return kpsPrimeResult{}
	}
	return res
}

// kpsRunOwnPrime shells out `kb prime --project <p> --limit 3` (plain
// text, unchanged behavior from the bare shell command it replaces) and
// returns the captured stdout. cmd.Dir is set to cwd so behavior matches
// the original bare shell invocation, which ran with the session's cwd.
func kpsRunOwnPrime(ctx context.Context, binary, proj, cwd string) string {
	cmd := osexec.CommandContext(ctx, binary, "prime", "--project", proj, "--limit", "3") //nolint:gosec
	if cwd != "" {
		cmd.Dir = cwd
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// kpsRenderSiblingSection builds the "## Sibling Activity" block from
// headline-per-project pairs, skipping projects with no headline and
// enforcing both the per-title cap and the total section byte cap.
// Returns "" when there's nothing to report (all siblings empty, or no
// siblings at all).
func kpsRenderSiblingSection(headlines map[string]string, order []string) string {
	var lines []string
	for _, p := range order {
		h := strings.TrimSpace(headlines[p])
		if h == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- **%s**: %s", p, kpsTruncateRunes(h, kpsTitleCapRunes)))
	}
	if len(lines) == 0 {
		return ""
	}

	section := "## Sibling Activity\n" + strings.Join(lines, "\n")
	if len(section) > kpsSiblingSectionCapBytes {
		// Trim whole lines from the end until it fits, rather than cutting
		// mid-line — a truncated title mid-word is worse than one fewer line.
		for len(lines) > 0 && len("## Sibling Activity\n"+strings.Join(lines, "\n")) > kpsSiblingSectionCapBytes {
			lines = lines[:len(lines)-1]
		}
		if len(lines) == 0 {
			return ""
		}
		section = "## Sibling Activity\n" + strings.Join(lines, "\n")
	}
	return section
}

// cmdKbPrimeSiblings is the SessionStart hook entry point. See package
// doc comment for the full design.
func cmdKbPrimeSiblings() {
	in, _ := readStdinForHook()

	binary := resolveKbBinary()
	if binary == "" {
		return
	}

	ownProject, _ := project.Resolve(in.EffectiveCWD())

	ctx, cancel := context.WithTimeout(context.Background(), kpsOverallTimeout)
	defer cancel()

	siblings := kpsSiblingProjects(ownProject)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		ownOutput string
		headlines = make(map[string]string, len(siblings))
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		out := kpsRunOwnPrime(ctx, binary, ownProject, in.EffectiveCWD())
		mu.Lock()
		ownOutput = out
		mu.Unlock()
	}()

	for _, sib := range siblings {
		sib := sib
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, callCancel := context.WithTimeout(ctx, kpsPerCallTimeout)
			defer callCancel()
			res := kpsRunKbPrimeJSON(callCtx, binary, sib)
			h := res.headline()
			mu.Lock()
			headlines[sib] = h
			mu.Unlock()
		}()
	}

	wg.Wait()

	if ownOutput == "" {
		hooklog.Writef("kb-prime-siblings: own kb prime produced no output for project=%s", ownProject)
		return
	}

	combined := ownOutput
	if section := kpsRenderSiblingSection(headlines, siblings); section != "" {
		combined = combined + "\n\n" + section
	}

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": combined,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Println(string(b))
}
