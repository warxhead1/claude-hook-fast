// cmd_rules.go — data-driven per-project rules engine for Claude Code hooks.
//
// Subcommand: claude-hook-fast rules
//
// Evaluates TWO config layers per invocation (since 2026-07-11):
//
//  1. user-global ~/.claude/hook-rules.json — applies from EVERY cwd, so
//     cross-repo invariants (dispatch isolation, post-compaction freeze,
//     whole-tree git ops) cannot be evaded by dispatching from a project
//     that never opted in;
//  2. <project>/.claude/hook-rules.json (discovered by walking up from the
//     hook payload's cwd) — project-specific rules layered on top.
//
// All matching rules across both layers either block (exit 2) or pass
// through (exit 0). One Go process per tool call; everything else is config.
//
// Config schema (hook-rules.json version 1):
//
//	{
//	  "version": 1,
//	  "pipelines": [
//	    {
//	      "id":     "flow",
//	      "output": "~/.claude/hooks/log/flow/{project}/{date}.jsonl",
//	      "fields": ["ts","sid","agent","tool","class","cmd_120"]
//	    }
//	  ],
//	  "rules": [
//	    {
//	      "id":    "example",
//	      "event": "PreToolUse",
//	      "match": {
//	        "tool":          "Bash",          // "" = any tool
//	        "command_regex": "pattern",       // compiled RE; "" = skip
//	        "class":         "lifecycle",     // classifyTool result; "" = any
//	        "class_ne":      "",              // match when class NOT equal; "" = skip
//	        "cwd_regex":     "pattern",       // compiled RE on cwd; "" = skip
//	        "builtin":       ""               // "cargo_no_rust_edits" | ""
//	      },
//	      "state": {
//	        "counter":   "my_counter",        // state file name; "" = no counter
//	        "scope":     "session",           // only "session" supported
//	        "increment": true                 // if true, increment before when-check
//	      },
//	      "when": {
//	        "counter_gte": 2,                 // fire when counter >= N (0 = always)
//	        "counter_eq":  0,                 // fire when counter == N (0 = skip)
//	        "fire_once":   false              // reset counter to 0 after firing
//	      },
//	      "action": {
//	        "type":     "block",              // "block"|"ask"|"warn"|"log"|"emit"|"reset"
//	        "message":  "...",                // for block/warn
//	        "pipeline": "flow",               // for log/emit
//	        "counter":  ""                    // for reset: counter to zero out
//	      }
//	    }
//	  ]
//	}
//
// Action types:
//
//	"block"  — write message to stderr, exit 2 (stops Claude).
//	"ask"    — emit a PreToolUse permissionDecision "ask" with the message as
//	           the reason, exit 0: the operator approves or denies the call in
//	           the UI. Hook "ask" still prompts under bypassPermissions, where a
//	           settings.json ask rule does not.
//	"warn"   — surface message as a non-blocking feedback nudge (written to the
//	           feedback-db, collected by feedback-collector and shown to the
//	           assistant) and CONTINUE evaluating; exit 0. Use for steering rules
//	           that should make the agent AWARE without hard-stopping a justified
//	           action. (Previously an alias for "block" — split 2026-06-13.)
//	"log"    — append a record to the named pipeline file, exit 0.
//	"emit"   — alias for "log".
//	"reset"  — zero out the named counter (action.counter), exit 0.
//	           Used to reset a streak counter when a non-matching command fires.
//
// Built-in condition keywords (match.builtin):
//
//	"cargo_no_rust_edits"        — fires when cwd is inside a tengine worktree
//	                                AND git status shows no .rs/.c/.h/.toml edits.
//	"agent_no_isolation"         — fires when cwd is a shared (non-worktree)
//	                                git checkout AND the Agent/Task call's
//	                                tool_input has no non-blank isolation field.
//	                                Exempt: subagent_type in
//	                                nonClobberingAgentTypes (Explore, Plan,
//	                                report-writer).
//	"workflow_partial_isolation" — fires when a Workflow script's agent() call
//	                                count exceeds its isolation: 'worktree'
//	                                spec count, in a shared git checkout.
//	"duplicate_binding_spec"     — fires when a Write/Edit puts "BINDING" into
//	                                docs/design/<X>.md while a sibling spec
//	                                sharing X's feature prefix already claims
//	                                BINDING and is not marked SUPERSEDED.
//
// State directory: ~/.claude/hooks/state/<project-slug>/<session_id>/
// Config cache:    stat mtime — reparsed only on change.
// Failure mode:    any config/I/O error → pass-through exit 0 (never blocks).
// Latency target:  <10ms per benign invocation.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── config types ──────────────────────────────────────────────────────────────

type rulesConfig struct {
	Version   int           `json:"version"`
	Pipelines []pipelineCfg `json:"pipelines"`
	Rules     []ruleCfg     `json:"rules"`
}

type pipelineCfg struct {
	ID     string   `json:"id"`
	Output string   `json:"output"`
	Fields []string `json:"fields"`
}

type ruleCfg struct {
	ID     string    `json:"id"`
	Event  string    `json:"event"`
	Match  matchCfg  `json:"match"`
	State  stateCfg  `json:"state"`
	When   whenCfg   `json:"when"`
	Action actionCfg `json:"action"`
}

type matchCfg struct {
	Tool         string `json:"tool"`          // "" = any; exact match
	CommandRegex string `json:"command_regex"` // compiled lazily
	Class        string `json:"class"`         // "" = any; classifyTool result
	ClassNE      string `json:"class_ne"`      // "" = skip; fires when class != this
	CWDRegex     string `json:"cwd_regex"`     // compiled lazily; "" = skip
	Builtin      string `json:"builtin"`       // "cargo_no_rust_edits" | ""
}

type stateCfg struct {
	Counter    string `json:"counter"`
	Scope      string `json:"scope"`       // "session"
	Increment  bool   `json:"increment"`   // increment before evaluating when
	TTLSeconds int    `json:"ttl_seconds"` // 0 = no TTL; >0 = reset counter to 0 when its state file is older than this (last matching invocation was > TTL ago)
	Window     bool   `json:"window"`      // sliding window: state file = one unix-epoch line per ALLOWED invocation, entries age out after ttl_seconds individually; blocked attempts never consume or extend the window (requires ttl_seconds > 0)
}

type whenCfg struct {
	CounterGTE int  `json:"counter_gte"` // fire when counter >= N; 0 = always
	CounterEQ  int  `json:"counter_eq"`  // fire when counter == N; 0 = skip
	FireOnce   bool `json:"fire_once"`   // reset counter to 0 after firing
}

type actionCfg struct {
	Type     string `json:"type"` // "block"|"ask"|"warn"|"log"|"emit"|"reset"
	Message  string `json:"message"`
	Pipeline string `json:"pipeline"` // for log/emit
	Counter  string `json:"counter"`  // for reset
}

// ── config cache ──────────────────────────────────────────────────────────────

type configCache struct {
	mu      sync.Mutex
	path    string
	mtime   time.Time
	cfg     *rulesConfig
	cmdRegs map[int]*regexp.Regexp
	cwdRegs map[int]*regexp.Regexp
}

// configCaches holds one cache per config path — the engine can evaluate a
// user-global layer (~/.claude/hook-rules.json) plus a project layer in the
// same invocation, and each keeps its own mtime-keyed parse cache.
var configCaches sync.Map // path string -> *configCache

func loadRulesConfig(path string) (*rulesConfig, map[int]*regexp.Regexp, map[int]*regexp.Regexp, error) {
	v, _ := configCaches.LoadOrStore(path, &configCache{})
	return v.(*configCache).load(path)
}

func (c *configCache) load(path string) (*rulesConfig, map[int]*regexp.Regexp, map[int]*regexp.Regexp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if c.cfg != nil && c.path == path && !fi.ModTime().After(c.mtime) {
		return c.cfg, c.cmdRegs, c.cwdRegs, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	var cfg rulesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, nil, err
	}

	cmdRegs := make(map[int]*regexp.Regexp, len(cfg.Rules))
	cwdRegs := make(map[int]*regexp.Regexp, len(cfg.Rules))
	for i, r := range cfg.Rules {
		if r.Match.CommandRegex != "" {
			re, err := regexp.Compile(r.Match.CommandRegex)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("rule %q command_regex: %w", r.ID, err)
			}
			cmdRegs[i] = re
		}
		if r.Match.CWDRegex != "" {
			re, err := regexp.Compile(r.Match.CWDRegex)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("rule %q cwd_regex: %w", r.ID, err)
			}
			cwdRegs[i] = re
		}
	}

	c.path = path
	c.mtime = fi.ModTime()
	c.cfg = &cfg
	c.cmdRegs = cmdRegs
	c.cwdRegs = cwdRegs
	return c.cfg, c.cmdRegs, c.cwdRegs, nil
}

// ── config discovery ──────────────────────────────────────────────────────────

// findRulesConfig walks up from startDir looking for .claude/hook-rules.json.
func findRulesConfig(startDir string) string {
	// The walk-up may reach $HOME (that is how project discovery finds and
	// dedupes the user-global config) but never climbs above it: nothing
	// above a home directory is a project, and stopping there keeps test
	// sandboxes with TMPDIR under the real home from leaking the real
	// global config into their walks.
	home, _ := os.UserHomeDir()
	dir := startDir
	for i := 0; i < 20; i++ {
		candidate := filepath.Join(dir, ".claude", "hook-rules.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if home != "" && dir == home {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// ── state files ───────────────────────────────────────────────────────────────

// projectSlug converts a project path to a filesystem-safe name (max 64 chars).
func projectSlug(projectDir string) string {
	slug := strings.NewReplacer("/", "_", ".", "_", " ", "_").Replace(strings.TrimLeft(projectDir, "/"))
	if len(slug) > 64 {
		slug = slug[len(slug)-64:]
	}
	return slug
}

// stateDir returns ~/.claude/hooks/state/<slug>/<session_id>/
func stateDir(projectDir, sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".claude", "hooks", "state", projectSlug(projectDir), sessionID)
}

func readCounter(dir, name string) int {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

func writeCounter(dir, name string, n int) {
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(n)), 0o644)
}

// counterStale reports whether the counter's state file has not been touched
// within ttl. The file's mtime tracks the last matching invocation (each
// increment rewrites it), so "stale" means "no matching activity in ttl" — used
// for time-based counter resets that survive across a single session. A missing
// file is not stale (readCounter already yields 0).
func counterStale(dir, name string, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) > ttl
}

// ── sliding-window counters ───────────────────────────────────────────────────
//
// A window counter ("state": {"window": true}) stores one unix-epoch line per
// ALLOWED matching invocation instead of a bare count, so "N in the last T
// minutes" is literally true: entries age out individually, and a blocked
// attempt neither consumes a slot nor extends the window. The legacy bare
// counter did both — every blocked retry rewrote the file, refreshing the
// mtime that the TTL reset keyed on, so a lockout perpetuated itself under
// retries (2026-07-09 multi-agent wave lockout).
//
// Legacy bare-int content parses as a single tiny "epoch" that prunes out
// immediately — a deliberate one-time amnesty at upgrade (an old count carries
// no timestamps to age out).
//
// Concurrency: read-prune-write is not atomic across concurrent hook
// invocations (neither was the legacy counter); worst case is one lost or
// double-counted entry that ages out within ttl_seconds.

func readWindow(dir, name string) []int64 {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	var out []int64
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return nil // unparsable → legacy/corrupt → empty window
		}
		out = append(out, n)
	}
	return out
}

func pruneWindow(entries []int64, ttl time.Duration, now time.Time) []int64 {
	if ttl <= 0 {
		return nil // a window without a TTL is meaningless — fail open
	}
	cutoff := now.Add(-ttl).Unix()
	var out []int64 // fresh slice — never alias/mutate the caller's input
	for _, e := range entries {
		if e > cutoff {
			out = append(out, e)
		}
	}
	return out
}

func writeWindow(dir, name string, entries []int64) {
	_ = os.MkdirAll(dir, 0o755)
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%d\n", e)
	}
	_ = os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644)
}

// resetRuleCounter zeroes a rule's counter in whichever format it uses.
func resetRuleCounter(sd string, rule ruleCfg) {
	if rule.State.Window {
		writeWindow(sd, rule.State.Counter, nil)
	} else {
		writeCounter(sd, rule.State.Counter, 0)
	}
}

// ── built-in condition: cargo_no_rust_edits ───────────────────────────────────

// mainRootFor returns the effective repo root to use for worktree checks.
// TENGINE_MAIN_ROOT env overrides (used in tests); otherwise the projectDir.
func mainRootFor(projectDir string) string {
	if v := os.Getenv("TENGINE_MAIN_ROOT"); v != "" {
		return v
	}
	return projectDir
}

// checkCargoNoRustEditsTimeout bounds the git status call below. This is the
// only subprocess call on the "rules" PreToolUse hot path (fires on every
// tool call across every worktree cwd) — every other exec.Command in this
// repo uses exec.CommandContext per convention (see cmd_session_registry.go),
// but this one didn't, and a `git status` can hang indefinitely on a
// contended index.lock under heavy concurrent multi-agent/worktree load.
// An unbounded hang here blocks the tool call itself, not just Stop — unlike
// the Node hooks under timeout-wrapper.sh, there is no SIGKILL-swallow net
// on this path, so a stuck git call can abort the turn (tachyonac-engine,
// 2026-08-03).
const checkCargoNoRustEditsTimeout = 2 * time.Second

// checkCargoNoRustEdits returns true (condition holds → should block) when the
// cwd is inside a tengine worktree AND git status shows no Rust/C/TOML edits.
// Git failure (e.g. directory doesn't exist, or the call timed out) is
// treated as "no edits" → block, matching the bash flow-guard.sh behavior
// (git pipe to wc -l returns 0).
func checkCargoNoRustEdits(cwd, projectDir string) bool {
	root := mainRootFor(projectDir)
	wtBase := filepath.Join(root, ".claude", "worktrees")
	if !strings.HasPrefix(cwd, wtBase) {
		return false // not in a worktree → condition does not hold
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkCargoNoRustEditsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain").Output() //nolint:gosec
	if err != nil {
		// git failure (including timeout) → no edits visible → should block
		// (matches bash behavior: wc -l of empty pipe = 0 → wt_modified == 0 → block)
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		fname := parts[len(parts)-1]
		switch filepath.Ext(fname) {
		case ".rs", ".c", ".h", ".toml":
			return false // has Rust/C edits → cargo allowed
		}
		if filepath.Base(fname) == "build.rs" {
			return false
		}
	}
	return true // no Rust/C edits → block
}

// ── built-in condition: bash_write_guard ─────────────────────────────────────

// checkBashWriteGuard returns true (should block) when:
//   - cwd is inside a tengine worktree
//   - the command writes to a path under mainRoot that is NOT itself a worktree
//
// Mirrors detector (e) in flow-guard.sh.
func checkBashWriteGuard(cmd, cwd, projectDir string) bool {
	root := mainRootFor(projectDir)
	wtBase := filepath.Join(root, ".claude", "worktrees")
	if !strings.HasPrefix(cwd, wtBase) {
		return false // not from a worktree
	}
	// Helper: does the token look like a main-tree path but not a worktree path?
	isMainWrite := func(path string) bool {
		return strings.HasPrefix(path, root+"/") &&
			!strings.HasPrefix(path, wtBase+"/")
	}

	// Check 1: sed -i targeting main tree
	if reSedI.MatchString(cmd) {
		for _, token := range strings.Fields(cmd) {
			if isMainWrite(token) {
				return true
			}
		}
	}
	// Check 2: tee to main tree
	if m := reTeeCapture.FindStringSubmatch(cmd); m != nil && isMainWrite(m[1]) {
		return true
	}
	// Check 3: redirect (> or >>) to main tree
	if m := reRedirectCapture.FindStringSubmatch(cmd); m != nil && isMainWrite(m[1]) {
		return true
	}
	// Check 4: cp/mv/rsync — last token is the destination
	if reCpMvRsync.MatchString(cmd) {
		tokens := strings.Fields(cmd)
		if len(tokens) > 0 {
			last := tokens[len(tokens)-1]
			if isMainWrite(last) {
				return true
			}
		}
	}
	// Check 5: git -C <main-tree> commit/apply/checkout/restore
	if m := reGitCMain.FindStringSubmatch(cmd); m != nil {
		gitTarget := m[1]
		if strings.HasPrefix(gitTarget, root) &&
			!strings.HasPrefix(gitTarget, wtBase) &&
			reGitWriteCmd.MatchString(cmd) {
			return true
		}
	}
	return false
}

var (
	reSedI            = regexp.MustCompile(`\bsed\s+-i\b`)
	reTeeCapture      = regexp.MustCompile(`\btee\s+(\S+)`)
	reRedirectCapture = regexp.MustCompile(`[>]{1,2}\s*(\S+)`)
	reCpMvRsync       = regexp.MustCompile(`\b(cp|mv|rsync)\b`)
	reGitCMain        = regexp.MustCompile(`\bgit\s+-C\s+(\S+)`)
	reGitWriteCmd     = regexp.MustCompile(`\b(commit|apply|checkout|restore)\b`)
)

// ── built-in condition: agent_no_isolation ───────────────────────────────────

// isGitRepo reports whether dir is inside a git checkout (regular repo or
// worktree). Delegates to repoOf — the one canonical walker — which also
// rejects phantom repos from stray empty .git dirs (the /tmp/.git incident).
func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	_, kind := repoOf(dir)
	return kind != ""
}

// alreadyIsolatedCWD reports whether cwd is already inside a Claude-managed
// per-agent worktree checkout, in which case a dispatch made FROM there
// doesn't need its own nested isolation — it's already sandboxed by
// whatever created that worktree. Deliberately does NOT treat a generic
// <project>/<branch> bare-repo layout (e.g. hearth-loom/main) as isolated —
// that reflects the operator's own branch organization, not per-dispatch
// agent isolation.
func alreadyIsolatedCWD(cwd string) bool {
	sep := string(filepath.Separator)
	return strings.Contains(cwd, sep+".claude"+sep+"worktrees"+sep) ||
		strings.Contains(cwd, sep+".worktrees"+sep)
}

// checkAgentNoIsolation returns true (condition holds → should block) when
// cwd sits in a shared, non-isolated git checkout and toolInput (the
// Agent/Task call's parameters) has no non-blank isolation field.
//
// Real incident, 2026-07-02: an Agent dispatched without isolation into
// hearth's shared main checkout hit an "uncommitted changes" obstacle and
// ran `git stash`, silently hiding 450+ lines of unrelated WIP directly on
// main. This builtin exists to catch that class of dispatch before it runs.
func checkAgentNoIsolation(cwd string, toolInput json.RawMessage) bool {
	if !isGitRepo(cwd) || alreadyIsolatedCWD(cwd) {
		return false
	}
	if len(toolInput) == 0 {
		return false // fail-open: no input to inspect
	}
	var ti map[string]any
	if err := json.Unmarshal(toolInput, &ti); err != nil {
		return false // fail-open: malformed input
	}
	if st, _ := ti["subagent_type"].(string); nonClobberingAgentTypes[strings.TrimSpace(st)] {
		return false // cannot clobber a shared checkout (no Edit, no Bash)
	}
	isolation, _ := ti["isolation"].(string)
	return strings.TrimSpace(isolation) == ""
}

// nonClobberingAgentTypes are subagent types that cannot clobber concurrent
// uncommitted work in a shared checkout, so they are exempt from the isolation
// requirement. Two admissible shapes:
//
//   - Strictly read-only (Explore, Plan): no Write/Edit/NotebookEdit at all.
//   - Report-delivering (report-writer): Write but NO Edit and NO Bash. It can
//     create a new .md report in the shared checkout — the whole point of the
//     dispatch — but has no `git stash`, no `sed -i`, no `rm`, and no in-place
//     modification of an existing file. The 2026-07-02 stash incident and the
//     2026-07-11 overwrite both needed Bash or Edit; neither is reachable here.
//
// Deliberately narrow: types with "All tools" or Bash access (general-purpose,
// sonnet-worker, haiku-grunt, claude-code-guide, …) stay guarded — Bash can write.
//
// A name here is only half the contract; the other half is the agent definition
// at ~/.claude/agents/<name>.md, whose `tools:` frontmatter is what actually
// removes Edit/Bash. Adding a name to this map without that definition (or
// widening that definition's tools) silently reopens the hole.
var nonClobberingAgentTypes = map[string]bool{
	"Explore":       true,
	"Plan":          true,
	"report-writer": true,
}

// knownBuiltins lists every builtin this binary implements. A rule naming any
// other builtin is skipped entirely (fail-open) instead of matching
// unconditionally — configs may be written for newer binaries than the one
// installed.
var knownBuiltins = map[string]bool{
	"cargo_no_rust_edits":        true,
	"bash_write_guard":           true,
	"agent_no_isolation":         true,
	"workflow_partial_isolation": true,
	"duplicate_binding_spec":     true,
}

// checkWorkflowPartialIsolation returns true (should block) when a Workflow
// script's tool_input has fewer isolation: 'worktree' specs than agent()
// calls, while cwd sits in a shared, non-isolated git checkout. Mirrors
// dispatch-model-guard's guardWorkflowModels coverage-counting approach.
func checkWorkflowPartialIsolation(cwd string, toolInput json.RawMessage) bool {
	if !isGitRepo(cwd) || alreadyIsolatedCWD(cwd) {
		return false
	}
	if len(toolInput) == 0 {
		return false
	}
	var ti map[string]any
	if err := json.Unmarshal(toolInput, &ti); err != nil {
		return false
	}
	script, _ := ti["script"].(string)
	if script == "" {
		return false
	}
	agentCount := strings.Count(script, "agent(")
	isolationCount := strings.Count(script, `isolation: "worktree"`) + strings.Count(script, `isolation: 'worktree'`)
	return agentCount > 0 && isolationCount < agentCount
}

// ── built-in condition: duplicate_binding_spec ───────────────────────────────

// checkDuplicateBindingSpec returns true (should block) when a Write/Edit
// puts the word BINDING into a docs/design/*.md file while a sibling spec
// sharing the same feature prefix (first two hyphen tokens of the filename,
// e.g. HERMES-H3-*) already claims BINDING and is not marked SUPERSEDED.
//
// Real incident, 2026-07-11: two docs both headed "BINDING build spec"
// coexisted for one feature; a post-compaction session picked up the second
// doc, concluded the work was unbuilt, and dispatched a duplicate agent wave
// into territory the first wave was actively editing. Exactly one live
// BINDING spec per feature prefix is the invariant this enforces.
func checkDuplicateBindingSpec(toolInput json.RawMessage) bool {
	if len(toolInput) == 0 {
		return false // fail-open
	}
	var ti struct {
		FilePath  string `json:"file_path"`
		Content   string `json:"content"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(toolInput, &ti); err != nil {
		return false // fail-open
	}
	sep := string(filepath.Separator)
	if ti.FilePath == "" || !strings.HasSuffix(ti.FilePath, ".md") ||
		!strings.Contains(ti.FilePath, sep+"docs"+sep+"design"+sep) {
		return false
	}
	body := ti.Content
	if body == "" {
		body = ti.NewString
	}
	if !strings.Contains(body, "BINDING") {
		return false
	}
	prefix := bindingSpecPrefix(filepath.Base(ti.FilePath))
	if prefix == "" {
		return false
	}
	dir := filepath.Dir(ti.FilePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false // fail-open
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == filepath.Base(ti.FilePath) ||
			!strings.HasSuffix(name, ".md") || !strings.HasPrefix(name, prefix+"-") {
			continue
		}
		if checked++; checked > 50 {
			return false // absurd sibling count — fail open rather than stall the hook
		}
		data, err := readFileBounded(filepath.Join(dir, name), 256<<10)
		if err != nil {
			continue
		}
		if strings.Contains(data, "BINDING") && !strings.Contains(data, "SUPERSEDED") {
			return true
		}
	}
	return false
}

// bindingSpecPrefix derives the shared feature prefix from a spec filename:
// the first two hyphen-separated tokens ("HERMES-H3-INVESTIGATE-DECIDE.md" →
// "HERMES-H3"). Filenames with fewer than three tokens have no inferable
// shared prefix and are never treated as duplicates.
func bindingSpecPrefix(base string) string {
	name := strings.TrimSuffix(base, ".md")
	parts := strings.SplitN(name, "-", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[0] + "-" + parts[1]
}

func readFileBounded(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit))
	return string(data), err
}

// ── pipeline output ───────────────────────────────────────────────────────────

func expandOutputPath(template, projectDir string) string {
	out := strings.ReplaceAll(template, "{project}", projectSlug(projectDir))
	out = strings.ReplaceAll(out, "{date}", time.Now().UTC().Format("2006-01-02"))
	if strings.HasPrefix(out, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			out = filepath.Join(home, out[2:])
		}
	}
	return out
}

func buildPipelineRecord(fields []string, in *ruleHookPayload, projectDir string) map[string]any {
	rec := make(map[string]any, len(fields))
	for _, f := range fields {
		switch f {
		case "ts":
			rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
		case "sid":
			rec["sid"] = in.SessionID
		case "agent":
			rec["agent"] = in.agentKind()
		case "tool":
			rec["tool"] = in.ToolName
		case "class":
			rec["class"] = classifyTool(in.ToolName, in.command())
		case "cmd_120":
			rec["cmd_120"] = first120(in.command())
		case "project":
			rec["project"] = projectSlug(projectDir)
		case "dispatch":
			// Agent/Task dispatch tags: who was spawned, as what, on which model.
			if (in.ToolName == "Agent" || in.ToolName == "Task") && len(in.ToolInput) > 0 {
				var ti map[string]any
				if err := json.Unmarshal(in.ToolInput, &ti); err == nil {
					tags := make(map[string]any, 4)
					for _, k := range []string{"name", "subagent_type", "model", "description"} {
						if v, ok := ti[k].(string); ok && v != "" {
							tags[k] = v
						}
					}
					if len(tags) > 0 {
						rec["dispatch"] = tags
					}
				}
			}
		}
	}
	return rec
}

func appendToPipeline(cfg pipelineCfg, in *ruleHookPayload, projectDir string) {
	path := expandOutputPath(cfg.Output, projectDir)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	rec := buildPipelineRecord(cfg.Fields, in, projectDir)
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// ── hook payload ──────────────────────────────────────────────────────────────

type ruleHookPayload struct {
	SessionID       string          `json:"session_id"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	CWD             string          `json:"cwd"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
}

func (h *ruleHookPayload) command() string {
	if h.ToolName != "Bash" || len(h.ToolInput) == 0 {
		return ""
	}
	var ti map[string]any
	if err := json.Unmarshal(h.ToolInput, &ti); err != nil {
		return ""
	}
	cmd, _ := ti["command"].(string)
	return cmd
}

func (h *ruleHookPayload) agentKind() string {
	if h.ParentToolUseID != "" {
		return "host_subagent"
	}
	return "host_claude_code"
}

// stripQuotedSpans replaces the CONTENTS of single- and double-quoted spans
// with a single space, preserving the quote characters. command_regex rules
// match the result, so they see shell syntax — not string-literal text.
// Builtins (bash_write_guard) and classifyTool keep the raw command: they
// legitimately inspect quoted paths. Trade-off accepted: an invocation hidden
// inside quotes (bash -c "shadergen start x") is missed; a false BLOCK on
// prose costs a lockout, a false PASS costs one advisory miss.
func stripQuotedSpans(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			b.WriteByte(c)
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			b.WriteByte(' ')
			if j < len(s) {
				b.WriteByte('\'')
			}
			i = j
		case '"':
			b.WriteByte(c)
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				j++
			}
			b.WriteByte(' ')
			if j < len(s) {
				b.WriteByte('"')
			}
			i = j
		case '\\':
			b.WriteByte(c)
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ── main entry point ──────────────────────────────────────────────────────────

// cmdRules is the "rules" subcommand. It exits 2 when a blocking rule fires.
// All errors → exit 0 (never block the loomie on config problems).
func cmdRules() {
	// Global bypass: TENGINE_FLOW_GUARD_OFF=1 skips all rule evaluation.
	if os.Getenv("TENGINE_FLOW_GUARD_OFF") == "1" {
		hookExit(0)
	}

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) == 0 {
		hookExit(0)
	}

	var in ruleHookPayload
	if err := json.Unmarshal(raw, &in); err != nil || in.ToolName == "" {
		hookExit(0)
	}
	if in.SessionID == "" {
		in.SessionID = "unknown"
	}

	configPaths := rulesConfigPaths(in.CWD)
	if len(configPaths) == 0 {
		hookExit(0)
	}

	cmd := in.command()

	// Bypass hatch: every block message advertises TENGINE_FLOW_GUARD_OFF=1 as
	// the override, but the process-env check above can never see an env prefix
	// on the command being vetted (the hook runs in its own process). Honor the
	// hatch in the command text so the advertised escape actually works.
	if strings.Contains(cmd, "TENGINE_FLOW_GUARD_OFF=1") {
		hookExit(0)
	}

	// command_regex rules describe shell INVOCATIONS. Match against a copy with
	// quoted-span contents removed so a command that merely mentions a pattern
	// inside a string argument (a commit message, an agent briefing) cannot trip
	// an invocation detector or inflate its counter (false lockout, 2026-06-12).
	matchCmd := stripQuotedSpans(cmd)

	now := time.Now()

	// Evaluate every config layer in order (user-global first, then project).
	// A block inside any layer exits 2 immediately, which also discards all
	// staged window appends — blocked attempts never consume or extend a window.
	var flushes []windowFlush
	for _, p := range configPaths {
		flushes = append(flushes, evalRulesConfig(p, &in, cmd, matchCmd, now)...)
	}
	for _, fl := range flushes {
		writeWindow(fl.sd, fl.name, append(fl.entries, now.Unix()))
	}
	hookExit(0)
}

// rulesConfigPaths resolves which config layers apply to this invocation:
//   - CLAUDE_HOOK_RULES_PATH pins exactly one config (conformance test / CI),
//     no layering.
//   - Otherwise: the user-global ~/.claude/hook-rules.json (if present)
//     PLUS the nearest project .claude/hook-rules.json found walking up from
//     cwd. Before 2026-07-11 a project config SHADOWED the global one — a
//     dispatch made from a config-less project cwd into another repo evaded
//     every isolation rule that repo had configured. Global rules now apply
//     from every cwd; project rules layer on top.
func rulesConfigPaths(cwd string) []string {
	if pinned := os.Getenv("CLAUDE_HOOK_RULES_PATH"); pinned != "" {
		return []string{pinned}
	}
	startDir := cwd
	if startDir == "" {
		if wd, err := os.Getwd(); err == nil {
			startDir = wd
		}
	}
	project := findRulesConfig(startDir)
	global := ""
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, ".claude", "hook-rules.json")
		if _, err := os.Stat(cand); err == nil {
			global = cand
		}
	}
	var out []string
	if global != "" {
		out = append(out, global)
	}
	if project != "" && project != global {
		out = append(out, project)
	}
	return out
}

// windowFlush is a staged sliding-window append, persisted only when the
// whole invocation (across all config layers) is allowed.
type windowFlush struct {
	sd      string
	name    string
	entries []int64
}

// evalRulesConfig evaluates one config file's rules against the payload.
// Blocking rules exit(2) inline; non-blocking outcomes accumulate. Returns
// the staged window appends for the caller to flush if nothing blocked.
func evalRulesConfig(configPath string, in *ruleHookPayload, cmd, matchCmd string, now time.Time) []windowFlush {
	// projectDir = the directory that contains .claude/
	projectDir := filepath.Dir(filepath.Dir(configPath))

	cfg, cmdRegs, cwdRegs, err := loadRulesConfig(configPath)
	if err != nil {
		return nil // malformed config → pass-through
	}

	pipelines := make(map[string]pipelineCfg, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		pipelines[p.ID] = p
	}

	sd := stateDir(projectDir, in.SessionID)

	// Sliding-window counter bookkeeping (see the window branch below): staged
	// appends are flushed only if no rule blocks this invocation.
	windowEntries := map[string][]int64{}
	windowStaged := map[string]bool{}
	windowTTLs := map[string]time.Duration{}

	for i, rule := range cfg.Rules {
		// ── match phase ────────────────────────────────────────────────────

		if rule.Match.Tool != "" && rule.Match.Tool != in.ToolName {
			continue
		}
		if cr := cwdRegs[i]; cr != nil && !cr.MatchString(in.CWD) {
			continue
		}
		if re := cmdRegs[i]; re != nil && !re.MatchString(matchCmd) {
			continue
		}
		if rule.Match.Class != "" && classifyTool(in.ToolName, cmd) != rule.Match.Class {
			continue
		}
		if rule.Match.ClassNE != "" && classifyTool(in.ToolName, cmd) == rule.Match.ClassNE {
			continue
		}
		if rule.Match.Builtin == "cargo_no_rust_edits" && !checkCargoNoRustEdits(in.CWD, projectDir) {
			continue
		}
		if rule.Match.Builtin == "bash_write_guard" && !checkBashWriteGuard(cmd, in.CWD, projectDir) {
			continue
		}
		if rule.Match.Builtin == "agent_no_isolation" && !checkAgentNoIsolation(in.CWD, in.ToolInput) {
			continue
		}
		if rule.Match.Builtin == "workflow_partial_isolation" && !checkWorkflowPartialIsolation(in.CWD, in.ToolInput) {
			continue
		}
		if rule.Match.Builtin == "duplicate_binding_spec" && !checkDuplicateBindingSpec(in.ToolInput) {
			continue
		}
		// Unknown builtin → the rule can never legitimately match. Without this,
		// a config written for a NEWER binary makes the rule fire UNCONDITIONALLY
		// on every matching tool call (live incident 2026-07-11: a new global
		// config blocked every Edit until the binary caught up). Fail open.
		if b := rule.Match.Builtin; b != "" && !knownBuiltins[b] {
			continue
		}

		// ── state / counter ────────────────────────────────────────────────

		counter := 0
		switch {
		case rule.State.Counter != "" && rule.State.Window:
			// Sliding-window counter (see helpers above): prune aged entries,
			// STAGE the current invocation, and persist only if the whole
			// invocation is allowed (flush before the final exit). A blocked
			// attempt must neither consume a slot nor extend the window.
			ttl := time.Duration(rule.State.TTLSeconds) * time.Second
			entries := pruneWindow(readWindow(sd, rule.State.Counter), ttl, now)
			windowEntries[rule.State.Counter] = entries
			windowTTLs[rule.State.Counter] = ttl
			if rule.State.Increment {
				windowStaged[rule.State.Counter] = true
			}
			counter = len(entries)
			if windowStaged[rule.State.Counter] {
				counter++ // count the staged current attempt, like the legacy pre-increment
			}
		case rule.State.Counter != "":
			// Time-based reset: if the counter's state file hasn't been touched
			// within ttl_seconds, the last matching invocation was long enough ago
			// that we zero the counter before evaluating. This lets per-session
			// guards recover after a quiet period without ending the session —
			// requested 2026-06-19. NB: every matching invocation (including ones
			// a later rule blocks) rewrites the file and so restarts this clock;
			// rate limiters should prefer "window": true, which has true
			// sliding-window semantics (2026-07-09 lockout).
			if rule.State.TTLSeconds > 0 &&
				counterStale(sd, rule.State.Counter,
					time.Duration(rule.State.TTLSeconds)*time.Second) {
				writeCounter(sd, rule.State.Counter, 0)
			}
			if rule.State.Increment {
				counter = readCounter(sd, rule.State.Counter) + 1
				writeCounter(sd, rule.State.Counter, counter)
			} else {
				counter = readCounter(sd, rule.State.Counter)
			}
		}

		// ── when condition ─────────────────────────────────────────────────

		if rule.When.CounterEQ != 0 && counter != rule.When.CounterEQ {
			continue
		}
		if rule.When.CounterGTE != 0 && counter < rule.When.CounterGTE {
			continue
		}

		// ── action ─────────────────────────────────────────────────────────

		switch rule.Action.Type {
		case "block":
			if rule.When.FireOnce && rule.State.Counter != "" {
				resetRuleCounter(sd, rule)
			}
			msg := rule.Action.Message
			// A window rule can say exactly when the next slot frees — the old
			// "resets after a quiet period" prose was unkeepable because blocked
			// attempts refreshed the reset clock (2026-07-09 wave lockout).
			if rule.State.Window {
				if entries := windowEntries[rule.State.Counter]; len(entries) > 0 {
					freeAt := time.Unix(entries[0], 0).Add(windowTTLs[rule.State.Counter])
					msg += fmt.Sprintf("\n(window: %d counted; oldest ages out in %s at %s; blocked attempts do NOT extend the window)",
						len(entries), time.Until(freeAt).Round(time.Second), freeAt.Format("15:04:05"))
				}
			}
			fmt.Fprint(os.Stderr, msg)
			if !strings.HasSuffix(msg, "\n") {
				fmt.Fprintln(os.Stderr)
			}
			hookExit(2)

		case "ask":
			if rule.When.FireOnce && rule.State.Counter != "" {
				resetRuleCounter(sd, rule)
			}
			out, _ := json.Marshal(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":            "PreToolUse",
					"permissionDecision":       "ask",
					"permissionDecisionReason": rule.Action.Message,
				},
			})
			fmt.Fprintln(os.Stdout, string(out))
			hookExit(0)

		case "warn":
			// Non-blocking steering nudge: surface the message via the feedback-db
			// (collected by the feedback-collector PostToolUse hook, shown to the
			// assistant) and CONTINUE evaluating. Reserves exit 2 for "block" so a
			// rule can make the agent AWARE without hard-stopping a justified action.
			// addFeedbackMessage lives in cmd_tengine_guards.go (same package).
			if rule.When.FireOnce && rule.State.Counter != "" {
				resetRuleCounter(sd, rule)
			}
			addFeedbackMessage(in.SessionID, "rules:"+rule.ID, rule.Action.Message, "warning")
			// non-blocking: continue evaluating rules

		case "log", "emit":
			if pl, ok := pipelines[rule.Action.Pipeline]; ok {
				appendToPipeline(pl, in, projectDir)
			}
			// non-blocking: continue evaluating rules

		case "reset":
			cname := rule.Action.Counter
			if cname == "" {
				cname = rule.State.Counter
			}
			if cname != "" {
				writeCounter(sd, cname, 0)
			}
			// non-blocking: continue evaluating rules
		}
	}

	// Stage sliding-window entries for the caller — flushed only when no rule
	// in ANY config layer blocked, so blocked attempts never consume or extend
	// a window.
	var flushes []windowFlush
	for name, staged := range windowStaged {
		if staged {
			flushes = append(flushes, windowFlush{sd: sd, name: name, entries: windowEntries[name]})
		}
	}
	return flushes
}
