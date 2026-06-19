// cmd_rules.go — data-driven per-project rules engine for Claude Code hooks.
//
// Subcommand: claude-hook-fast rules
//
// Reads <project>/.claude/hook-rules.json (discovered by walking up from the
// hook payload's cwd), evaluates all matching rules, and either blocks
// (exit 2) or passes through (exit 0). One Go process per tool call;
// everything else is config.
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
//	        "type":     "block",              // "block"|"warn"|"log"|"emit"|"reset"
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
//	"cargo_no_rust_edits" — fires when cwd is inside a tengine worktree AND
//	                         git status shows no .rs/.c/.h/.toml edits.
//
// State directory: ~/.claude/hooks/state/<project-slug>/<session_id>/
// Config cache:    stat mtime — reparsed only on change.
// Failure mode:    any config/I/O error → pass-through exit 0 (never blocks).
// Latency target:  <10ms per benign invocation.

package main

import (
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
}

type whenCfg struct {
	CounterGTE int  `json:"counter_gte"` // fire when counter >= N; 0 = always
	CounterEQ  int  `json:"counter_eq"`  // fire when counter == N; 0 = skip
	FireOnce   bool `json:"fire_once"`   // reset counter to 0 after firing
}

type actionCfg struct {
	Type     string `json:"type"`     // "block"|"warn"|"log"|"emit"|"reset"
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

var globalConfigCache = &configCache{}

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
	dir := startDir
	for i := 0; i < 20; i++ {
		candidate := filepath.Join(dir, ".claude", "hook-rules.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
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

// ── built-in condition: cargo_no_rust_edits ───────────────────────────────────

// mainRootFor returns the effective repo root to use for worktree checks.
// TENGINE_MAIN_ROOT env overrides (used in tests); otherwise the projectDir.
func mainRootFor(projectDir string) string {
	if v := os.Getenv("TENGINE_MAIN_ROOT"); v != "" {
		return v
	}
	return projectDir
}

// checkCargoNoRustEdits returns true (condition holds → should block) when the
// cwd is inside a tengine worktree AND git status shows no Rust/C/TOML edits.
// Git failure (e.g. directory doesn't exist) is treated as "no edits" →
// block, matching the bash flow-guard.sh behavior (git pipe to wc -l returns 0).
func checkCargoNoRustEdits(cwd, projectDir string) bool {
	root := mainRootFor(projectDir)
	wtBase := filepath.Join(root, ".claude", "worktrees")
	if !strings.HasPrefix(cwd, wtBase) {
		return false // not in a worktree → condition does not hold
	}
	out, err := exec.Command("git", "-C", cwd, "status", "--porcelain").Output() //nolint:gosec
	if err != nil {
		// git failure → no edits visible → should block (matches bash behavior:
		// wc -l of empty pipe = 0 → wt_modified == 0 → block)
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
//  - cwd is inside a tengine worktree
//  - the command writes to a path under mainRoot that is NOT itself a worktree
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
		os.Exit(0)
	}

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) == 0 {
		os.Exit(0)
	}

	var in ruleHookPayload
	if err := json.Unmarshal(raw, &in); err != nil || in.ToolName == "" {
		os.Exit(0)
	}
	if in.SessionID == "" {
		in.SessionID = "unknown"
	}

	// CLAUDE_HOOK_RULES_PATH allows the caller to pin a config path directly,
	// skipping cwd-based discovery. Used by the conformance test and CI.
	configPath := os.Getenv("CLAUDE_HOOK_RULES_PATH")
	if configPath == "" {
		startDir := in.CWD
		if startDir == "" {
			if wd, err := os.Getwd(); err == nil {
				startDir = wd
			}
		}
		configPath = findRulesConfig(startDir)
	}
	if configPath == "" {
		os.Exit(0)
	}
	// projectDir = the directory that contains .claude/
	projectDir := filepath.Dir(filepath.Dir(configPath))

	cfg, cmdRegs, cwdRegs, err := globalConfigCache.load(configPath)
	if err != nil {
		os.Exit(0) // malformed config → pass-through
	}

	pipelines := make(map[string]pipelineCfg, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		pipelines[p.ID] = p
	}

	sd := stateDir(projectDir, in.SessionID)
	cmd := in.command()

	// Bypass hatch: every block message advertises TENGINE_FLOW_GUARD_OFF=1 as
	// the override, but the process-env check above can never see an env prefix
	// on the command being vetted (the hook runs in its own process). Honor the
	// hatch in the command text so the advertised escape actually works.
	if strings.Contains(cmd, "TENGINE_FLOW_GUARD_OFF=1") {
		os.Exit(0)
	}

	// command_regex rules describe shell INVOCATIONS. Match against a copy with
	// quoted-span contents removed so a command that merely mentions a pattern
	// inside a string argument (a commit message, an agent briefing) cannot trip
	// an invocation detector or inflate its counter (false lockout, 2026-06-12).
	matchCmd := stripQuotedSpans(cmd)

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

		// ── state / counter ────────────────────────────────────────────────

		counter := 0
		if rule.State.Counter != "" {
			// Time-based reset: if the counter's state file hasn't been touched
			// within ttl_seconds, the last matching invocation was long enough ago
			// that we zero the counter before evaluating. This lets per-session
			// guards (e.g. the cold-silo-start limiter) recover after a quiet period
			// without ending the session — requested 2026-06-19.
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
				writeCounter(sd, rule.State.Counter, 0)
			}
			fmt.Fprint(os.Stderr, rule.Action.Message)
			if !strings.HasSuffix(rule.Action.Message, "\n") {
				fmt.Fprintln(os.Stderr)
			}
			os.Exit(2)

		case "warn":
			// Non-blocking steering nudge: surface the message via the feedback-db
			// (collected by the feedback-collector PostToolUse hook, shown to the
			// assistant) and CONTINUE evaluating. Reserves exit 2 for "block" so a
			// rule can make the agent AWARE without hard-stopping a justified action.
			// addFeedbackMessage lives in cmd_tengine_guards.go (same package).
			if rule.When.FireOnce && rule.State.Counter != "" {
				writeCounter(sd, rule.State.Counter, 0)
			}
			addFeedbackMessage(in.SessionID, "rules:"+rule.ID, rule.Action.Message, "warning")
			// non-blocking: continue evaluating rules

		case "log", "emit":
			if pl, ok := pipelines[rule.Action.Pipeline]; ok {
				appendToPipeline(pl, &in, projectDir)
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

	os.Exit(0)
}
