package main

// cmd_coord_status.go — coord-status [--json] [--run <id>]: print the lane
// ledger that coord-wake maintains (docs/coordinator-inbox.md, A.5).

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// coordSilentAfter is how long without any message marks a lane silent.
const coordSilentAfter = 10 * time.Minute

func cmdCoordStatus() {
	fs := flag.NewFlagSet("coord-status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the ledger as JSON")
	run := fs.String("run", "", "run id (default: most recently updated ledger)")
	if err := fs.Parse(flag.Args()[1:]); err != nil {
		hookExit(2)
	}
	hookExit(coordStatus(coordStateRoot(), *run, *asJSON, os.Stdout, os.Stderr))
}

func coordStatus(root, run string, asJSON bool, out, errw io.Writer) int {
	dir := ""
	if run != "" {
		dir = filepath.Join(root, coordSafeName(run))
	} else {
		var newest time.Time
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			st, err := os.Stat(filepath.Join(root, e.Name(), "lanes.json"))
			if err == nil && st.ModTime().After(newest) {
				newest, dir = st.ModTime(), filepath.Join(root, e.Name())
			}
		}
	}
	if dir == "" {
		fmt.Fprintln(errw, "coord-status: no ledger found under", root)
		return 1
	}
	b, err := os.ReadFile(filepath.Join(dir, "lanes.json"))
	if err != nil {
		fmt.Fprintln(errw, "coord-status:", err)
		return 1
	}
	if asJSON {
		_, _ = out.Write(b)
		return 0
	}
	var l coordLedger
	if json.Unmarshal(b, &l) != nil {
		fmt.Fprintln(errw, "coord-status: corrupt ledger")
		return 1
	}
	fmt.Fprintf(out, "run %s  ledger updated %s\n", l.RunID, l.Updated)
	handles := make([]string, 0, len(l.Lanes))
	for h := range l.Lanes {
		handles = append(handles, h)
	}
	sort.Strings(handles)
	for _, h := range handles {
		ln := l.Lanes[h]
		state, age := "alive", "?"
		if t, err := time.Parse(time.RFC3339, ln.LastSeen); err == nil {
			d := time.Since(t).Round(time.Second)
			age = d.String()
			if d > coordSilentAfter {
				state = "SILENT"
			}
		}
		name := ln.Title
		if name == "" {
			name = h
		}
		fmt.Fprintf(out, "%-7s %-40s seen %s ago", state, coordTrunc(name, 40), age)
		if ln.Phase != "" {
			fmt.Fprintf(out, "  phase=%s", ln.Phase)
		}
		if ln.LastStatus.Subject != "" {
			fmt.Fprintf(out, "  last: %s", coordTrunc(ln.LastStatus.Subject, 80))
		}
		fmt.Fprintln(out)
	}
	if rf := coordLoadReap(dir); rf.Last != nil {
		fmt.Fprintf(out, "\nlast reap scan %s mode=%s\n", rf.Last.At, rf.Last.Mode)
		fmt.Fprint(out, coordReapText(rf.Last, time.Now(), true))
	}
	return 0
}
