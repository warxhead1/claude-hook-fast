package main

// coord_reap.go — coord-reap (docs/coordinator-inbox.md, component E):
// lanes that finished and landed retire themselves; lanes that finished but
// did not land are reported until the coordinator disposes of them.
//
// Never uses --force, never deletes a branch, never uses rm. Every failure
// path leaves the worktree alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	coordReapDefaultGrace = 30 * time.Minute
	coordReapAgentRoot    = "/home/eric/data2/worktrees/"
	coordReapBudget       = 20 * time.Second
)

// coordReapWorker is the subset of a worker-list record the reaper uses.
type coordReapWorker struct {
	DispatchID    string `json:"dispatchId"`
	TaskID        string `json:"taskId"`
	RunID         string `json:"runId"`
	WorkerState   string `json:"workerState"`
	DispatchState string `json:"dispatchStatus"`
	TerminalState string `json:"terminalState"`
	Handle        string `json:"agentTerminalHandle"`
	Resource      struct {
		WorktreeID string `json:"worktreeId"`
	} `json:"resource"`
}

func (w coordReapWorker) path() string {
	_, after, ok := strings.Cut(w.Resource.WorktreeID, "::")
	if !ok {
		return ""
	}
	return after
}

func coordReapTerminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled"
}

// coordReapItem is one worktree's classification.
type coordReapItem struct {
	Title    string     `json:"title"`
	Path     string     `json:"path"`
	Kind     string     `json:"kind"` // orca | agent
	Dispatch []string   `json:"dispatches,omitempty"`
	Failing  []string   `json:"failing,omitempty"`
	Since    int64      `json:"since,omitempty"`
	Cmds     [][]string `json:"commands,omitempty"`
	Result   string     `json:"result,omitempty"`
	repo     string
	stop     []string
}

func (it coordReapItem) age(now time.Time) time.Duration {
	if it.Since == 0 {
		return 0
	}
	return now.Sub(time.Unix(it.Since, 0)).Round(time.Minute)
}

func (it coordReapItem) cmdLines() []string {
	var out []string
	for _, c := range it.Cmds {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// coordReapResult is the outcome of one scan (and apply).
type coordReapResult struct {
	At        string          `json:"at"`
	Mode      string          `json:"mode"`
	Note      string          `json:"note,omitempty"`
	Live      []string        `json:"skipped_live,omitempty"` // worktrees with a non-terminal worker
	Gone      int             `json:"terminal_worktree_gone,omitempty"`
	Reaped    []coordReapItem `json:"reaped,omitempty"`
	Reapable  []coordReapItem `json:"reapable,omitempty"`
	Festering []coordReapItem `json:"festering,omitempty"`
	Pending   []coordReapItem `json:"pending,omitempty"` // terminal and failing, inside the grace window
}

type coordReapFile struct {
	FirstSeen map[string]int64 `json:"first_seen"`
	Last      *coordReapResult `json:"last,omitempty"`
}

func coordLoadReap(dir string) coordReapFile {
	f := coordReapFile{FirstSeen: map[string]int64{}}
	if b, err := os.ReadFile(filepath.Join(dir, "reap.json")); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	if f.FirstSeen == nil {
		f.FirstSeen = map[string]int64{}
	}
	return f
}

func (f coordReapFile) save(dir string) {
	if b, err := json.MarshalIndent(f, "", "  "); err == nil {
		_ = coordWriteAtomic(filepath.Join(dir, "reap.json"), b)
	}
}

// ---------- git and /proc probes ----------

func coordReapGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %s", args[0], coordTrunc(strings.TrimSpace(errb.String()), 160))
	}
	return out.String(), nil
}

type coordProc struct {
	pid  int
	comm string
	cwd  string
}

func coordScanProcs() []coordProc {
	var out []coordProc
	ents, _ := os.ReadDir("/proc")
	self := os.Getpid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		out = append(out, coordProc{pid: pid, comm: strings.TrimSpace(string(comm)), cwd: cwd})
	}
	return out
}

func coordPathWithin(child, parent string) bool {
	child, parent = filepath.Clean(child), filepath.Clean(parent)
	return child == parent || strings.HasPrefix(child, parent+"/")
}

type coordReapProbe struct {
	cfg         coordConfig
	integration string
	procs       []coordProc
	cwd         string
	fetched     map[string]bool
}

// repoRoot returns the primary checkout of the repo owning worktree path p
// and whether p is itself a linked worktree (not the primary checkout).
func (pr *coordReapProbe) repoRoot(ctx context.Context, p string) (root string, linked bool, err error) {
	out, err := coordReapGit(ctx, p, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", false, err
	}
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return "", false, fmt.Errorf("rev-parse gave %q", out)
	}
	return filepath.Dir(lines[1]), lines[0] != lines[1], nil
}

// failing returns every condition that blocks a reap of worktree p (empty
// means reapable), or skip=true when p is not a linked worktree at all.
func (pr *coordReapProbe) failing(ctx context.Context, p string) (fails []string, repo string, skip bool) {
	root, linked, err := pr.repoRoot(ctx, p)
	if err != nil || !linked {
		return nil, "", true
	}
	if pr.integration == "" {
		return []string{"integration_branch unset"}, root, false
	}
	if st := strings.TrimSpace(mustOut(coordReapGit(ctx, p, "status", "--porcelain", "--untracked-files=all"))); st != "" {
		fails = append(fails, fmt.Sprintf("dirty with %d files", len(strings.Split(st, "\n"))))
	}
	if !pr.fetched[root] {
		pr.fetched[root] = true
		_, _ = coordReapGit(ctx, p, "fetch", "origin")
	}
	ref := "origin/" + pr.integration
	if _, err := coordReapGit(ctx, p, "merge-base", "--is-ancestor", "HEAD", ref); err != nil {
		n := strings.TrimSpace(mustOut(coordReapGit(ctx, p, "rev-list", "--count", ref+"..HEAD")))
		if n == "" {
			n = "unknown"
		}
		msg := fmt.Sprintf("%s commits not in %s", n, ref)
		if br := strings.TrimSpace(mustOut(coordReapGit(ctx, p, "symbolic-ref", "--short", "-q", "HEAD"))); br != "" {
			if _, err := coordReapGit(ctx, p, "rev-parse", "--verify", "-q", "origin/"+br); err != nil {
				msg += ", branch " + br + " unpushed"
			}
		}
		fails = append(fails, msg)
	}
	var holders []string
	for _, pc := range pr.procs {
		if coordPathWithin(pc.cwd, p) {
			holders = append(holders, fmt.Sprintf("%s(%d)", pc.comm, pc.pid))
		}
	}
	if len(holders) > 0 {
		sort.Strings(holders)
		if len(holders) > 4 {
			holders = append(holders[:4], "...")
		}
		fails = append(fails, "cwd in use by "+strings.Join(holders, " "))
	}
	if pr.cwd != "" && coordPathWithin(pr.cwd, p) {
		fails = append(fails, "contains the coordinator's cwd")
	}
	return fails, root, false
}

func mustOut(s string, _ error) string { return s }

// lastActivity is the newest mtime among the worktree dir, its top-level
// entries, and the worktree's HEAD and reflog (not the index: our own git status rewrites it).
func (pr *coordReapProbe) lastActivity(ctx context.Context, p string) time.Time {
	var newest time.Time
	touch := func(path string) {
		if st, err := os.Stat(path); err == nil && st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	touch(p)
	if ents, err := os.ReadDir(p); err == nil {
		for _, e := range ents {
			if e.Name() != ".git" {
				touch(filepath.Join(p, e.Name()))
			}
		}
	}
	if gd, err := coordReapGit(ctx, p, "rev-parse", "--absolute-git-dir"); err == nil {
		gd = strings.TrimSpace(gd)
		for _, f := range []string{"HEAD", "logs/HEAD"} {
			touch(filepath.Join(gd, f))
		}
	}
	return newest
}

// ---------- scan ----------

func coordReapMode(orders *coordOrders) string {
	switch orders.Reap {
	case "suggest", "off":
		return orders.Reap
	}
	return "auto"
}

func coordReapGrace(orders *coordOrders) time.Duration {
	if orders.ReapGraceMinutes > 0 {
		return time.Duration(orders.ReapGraceMinutes) * time.Minute
	}
	return coordReapDefaultGrace
}

func coordListWorkers(ctx context.Context, cfg coordConfig, runID string) ([]coordReapWorker, error) {
	out, err := coordOrca(ctx, cfg.OrcaBin, "orchestration", "worker-list", "--run", runID, "--limit", "100", "--json")
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var r struct {
		OK     bool `json:"ok"`
		Result struct {
			Workers []coordReapWorker `json:"workers"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &r) != nil || !r.OK {
		return nil, fmt.Errorf("worker-list unreadable")
	}
	return r.Result.Workers, nil
}

func coordReapTitle(l *coordLedger, w coordReapWorker, path string) string {
	for _, ln := range l.Lanes {
		if ln.Title != "" && (ln.DispatchID == w.DispatchID || (w.Handle != "" && ln.Handle == w.Handle)) {
			return ln.Title
		}
	}
	return filepath.Base(path)
}

// coordReapScan classifies every terminal worker's worktree plus the agent-*
// worktrees of the repos those workers touch. It runs no mutating command.
func coordReapScan(ctx context.Context, cfg coordConfig, runID, dir string, orders *coordOrders,
	saved coordReapFile, now time.Time) (*coordReapResult, map[string]int64) {
	res := &coordReapResult{At: now.UTC().Format(time.RFC3339), Mode: coordReapMode(orders)}
	seen := map[string]int64{}
	workers, err := coordListWorkers(ctx, cfg, runID)
	if err != nil {
		res.Note = "worker-list failed: " + err.Error()
		return res, saved.FirstSeen
	}
	if orders.IntegrationBranch == "" {
		res.Note = "integration_branch is unset in standing-orders.toml; nothing is reapable"
	}
	pr := &coordReapProbe{cfg: cfg, integration: orders.IntegrationBranch, procs: coordScanProcs(),
		cwd: cfg.ReapCwd, fetched: map[string]bool{}}
	if pr.cwd == "" {
		pr.cwd, _ = os.Getwd()
	}
	ledger := coordLoadLedger(dir, runID)

	// Group workers by worktree path; one live worker keeps the path out.
	type group struct {
		ws   []coordReapWorker
		live bool
	}
	groups := map[string]*group{}
	var order []string
	for _, w := range workers {
		p := w.path()
		if p == "" {
			continue
		}
		g := groups[p]
		if g == nil {
			g = &group{}
			groups[p] = g
			order = append(order, p)
		}
		g.ws = append(g.ws, w)
		if !coordReapTerminal(w.WorkerState) {
			g.live = true
		}
	}
	covered := map[string]bool{}
	repos := map[string]bool{}
	grace := coordReapGrace(orders)
	classify := func(it coordReapItem, fails []string, key string) {
		if len(fails) == 0 {
			it.Cmds = coordReapCommands(cfg, it)
			res.Reapable = append(res.Reapable, it)
			return
		}
		first, ok := saved.FirstSeen[key]
		if !ok {
			first = now.Unix()
		}
		seen[key] = first
		it.Failing, it.Since = fails, first
		if now.Sub(time.Unix(first, 0)) > grace {
			it.Cmds = coordReapDisposeCommands(cfg, it)
			res.Festering = append(res.Festering, it)
		} else {
			res.Pending = append(res.Pending, it)
		}
	}
	for _, p := range order {
		g := groups[p]
		covered[p] = true
		if g.live {
			res.Live = append(res.Live, filepath.Base(p))
			if root, _, err := pr.repoRoot(ctx, p); err == nil {
				repos[root] = true
			}
			continue
		}
		if ctx.Err() != nil {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			res.Gone++
			continue
		}
		it := coordReapItem{Path: p, Kind: "orca", Title: coordReapTitle(ledger, g.ws[0], p)}
		for _, w := range g.ws {
			it.Dispatch = append(it.Dispatch, w.DispatchID)
			if w.TerminalState != "released" {
				it.stop = append(it.stop, w.DispatchID)
			}
		}
		fails, repo, skip := pr.failing(ctx, p)
		if skip {
			continue
		}
		it.repo = repo
		repos[repo] = true
		classify(it, fails, p)
	}
	// agent-* worktrees of every repo a worker touched.
	var repoList []string
	for r := range repos {
		repoList = append(repoList, r)
	}
	sort.Strings(repoList)
	agentRoot := cfg.ReapAgentRoot
	if agentRoot == "" {
		agentRoot = coordReapAgentRoot
	}
	for _, repo := range repoList {
		out, err := coordReapGit(ctx, repo, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			p, ok := strings.CutPrefix(line, "worktree ")
			if !ok || covered[p] || !strings.HasPrefix(p, agentRoot) ||
				!strings.HasPrefix(filepath.Base(p), "agent-") || ctx.Err() != nil {
				continue
			}
			covered[p] = true
			fails, _, skip := pr.failing(ctx, p)
			if skip {
				continue
			}
			it := coordReapItem{Path: p, Kind: "agent", Title: filepath.Base(p), repo: repo}
			// A live agent session is not a festering lane: only report an
			// agent worktree nobody is using.
			inUse := false
			for _, f := range fails {
				if strings.HasPrefix(f, "cwd in use") {
					inUse = true
				}
			}
			// An in-process subagent's cwd is its parent session's, so /proc
			// cannot see it. A worktree touched inside the grace window is
			// treated as a live agent and left alone entirely.
			if inUse || now.Sub(pr.lastActivity(ctx, p)) < grace {
				continue
			}
			classify(it, fails, p)
		}
	}
	return res, seen
}

func coordReapCommands(cfg coordConfig, it coordReapItem) [][]string {
	var cmds [][]string
	if it.Kind == "orca" {
		for _, d := range it.stop {
			cmds = append(cmds, []string{cfg.OrcaBin, "orchestration", "worker-stop", "--dispatch", d, "--json"})
		}
		cmds = append(cmds, []string{cfg.OrcaBin, "worktree", "rm", "--worktree", "path:" + it.Path, "--json"})
	} else {
		cmds = append(cmds, []string{"git", "-C", it.repo, "worktree", "remove", it.Path})
	}
	return cmds
}

// coordReapDisposeCommands are the abandon commands; the merge path is text.
func coordReapDisposeCommands(cfg coordConfig, it coordReapItem) [][]string {
	return coordReapCommands(cfg, it)
}

// ---------- apply ----------

func coordReapRunCmd(ctx context.Context, argv []string) error {
	out, err := coordOrca(ctx, argv[0], argv[1:]...)
	if err != nil {
		return err
	}
	var r struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(out, &r) == nil && r.OK != nil && !*r.OK {
		return fmt.Errorf("not ok")
	}
	return nil
}

// coordReapApply runs each reapable item's commands in order, one worktree at
// a time, stopping at the first failure for that worktree.
func coordReapApply(ctx context.Context, dir string, res *coordReapResult) {
	var left []coordReapItem
	for _, it := range res.Reapable {
		if ctx.Err() != nil {
			left = append(left, it)
			continue
		}
		failed := ""
		for _, c := range it.Cmds {
			if err := coordReapRunCmd(ctx, c); err != nil {
				failed = fmt.Sprintf("%s: %v", strings.Join(c, " "), err)
				break
			}
		}
		if failed != "" {
			it.Result = "FAILED " + failed
			coordLog(dir, "reap FAILED path=%s %s", it.Path, failed)
			left = append(left, it)
			continue
		}
		it.Result = "reaped"
		coordLog(dir, "reap path=%s kind=%s dispatches=%s", it.Path, it.Kind, strings.Join(it.Dispatch, ","))
		res.Reaped = append(res.Reaped, it)
	}
	res.Reapable = left
}

// coordReap scans and, when the mode allows, applies. persist controls
// whether first-seen times and the last result are written back.
func coordReap(ctx context.Context, cfg coordConfig, runID, dir, modeOverride string, persist bool) *coordReapResult {
	orders := coordLoadOrders(coordRepoDir(cfg), dir)
	if modeOverride != "" {
		orders.Reap = modeOverride
	}
	saved := coordLoadReap(dir)
	if coordReapMode(orders) == "off" || os.Getenv("COORD_REAP_DISABLE") == "1" {
		return &coordReapResult{At: time.Now().UTC().Format(time.RFC3339), Mode: "off"}
	}
	res, seen := coordReapScan(ctx, cfg, runID, dir, orders, saved, time.Now())
	if res.Mode == "auto" {
		coordReapApply(ctx, dir, res)
	}
	if persist {
		prev := saved.Last
		if prev != nil {
			res.Reaped = append(append([]coordReapItem{}, prev.Reaped...), res.Reaped...)
			if len(res.Reaped) > 30 {
				res.Reaped = res.Reaped[len(res.Reaped)-30:]
			}
		}
		coordReapFile{FirstSeen: seen, Last: res}.save(dir)
	}
	return res
}

// ---------- reporting ----------

func coordReapSection(out *strings.Builder, label string, items []coordReapItem, now time.Time, cmds bool) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(out, "%s (%d)\n", label, len(items))
	for _, it := range items {
		fmt.Fprintf(out, "  - %s | %s", it.Title, it.Path)
		if len(it.Failing) > 0 {
			fmt.Fprintf(out, " | terminal/idle %s | %s", it.age(now), strings.Join(it.Failing, "; "))
		}
		if it.Result != "" {
			fmt.Fprintf(out, " | %s", it.Result)
		}
		fmt.Fprintln(out)
		if cmds {
			for _, c := range it.cmdLines() {
				fmt.Fprintf(out, "      %s\n", c)
			}
		}
	}
}

func coordReapText(res *coordReapResult, now time.Time, cmds bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "coord-reap mode=%s", res.Mode)
	if res.Note != "" {
		fmt.Fprintf(&b, " note: %s", res.Note)
	}
	b.WriteString("\n")
	if len(res.Live) > 0 || res.Gone > 0 {
		sort.Strings(res.Live)
		fmt.Fprintf(&b, "skipped: %d live (%s); %d terminal worker worktree(s) already gone\n",
			len(res.Live), strings.Join(res.Live, ", "), res.Gone)
	}
	coordReapSection(&b, "REAPED", res.Reaped, now, false)
	coordReapSection(&b, "REAPABLE", res.Reapable, now, cmds)
	coordReapSection(&b, "FESTERING", res.Festering, now, cmds)
	coordReapSection(&b, "PENDING (terminal, not landed, inside grace)", res.Pending, now, false)
	return b.String()
}

// coordReapBlockReason lists each festering lane with the two ways out.
func coordReapBlockReason(items []coordReapItem, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d finished lane(s) have not landed and are still holding a worktree. Decide each before ending the turn: merge it, or abandon it.\n", len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "- %s | %s | finished/idle %s | %s\n", it.Title, it.Path, it.age(now), strings.Join(it.Failing, "; "))
		fmt.Fprintf(&b, "  merge: land the branch into the integration branch and push it; the next Stop retires the lane.\n")
		fmt.Fprintf(&b, "  abandon (discards the worktree; the branch stays): %s\n", strings.Join(it.cmdLines(), " && "))
	}
	return b.String()
}

// coordReapSetHash identifies a set of items for once-per-set blocking.
func coordReapSetHash(items []coordReapItem) string {
	paths := make([]string, len(items))
	for i, it := range items {
		paths[i] = it.Path
	}
	sort.Strings(paths)
	return strings.Join(paths, ",")
}

func cmdCoordReap() {
	args := os.Args[2:]
	run, mode, asJSON, persist := "", "", false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--run":
			i++
			if i < len(args) {
				run = args[i]
			}
		case "--mode":
			i++
			if i < len(args) {
				mode = args[i]
			}
		case "--json":
			asJSON = true
		case "--persist":
			persist = true
		}
	}
	cfg := coordConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), coordReapBudget)
	defer cancel()
	if run == "" {
		run, _ = coordResolveRun(ctx, cfg)
	}
	if run == "" {
		fmt.Fprintln(os.Stderr, "coord-reap: no run (pass --run <id>)")
		hookExit(1)
	}
	if mode == "" {
		mode = "suggest"
	}
	dir := filepath.Join(cfg.StateRoot, coordSafeName(run))
	res := coordReap(ctx, cfg, run, dir, mode, persist)
	if asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Print(coordReapText(res, time.Now(), true))
	}
	hookExit(0)
}
