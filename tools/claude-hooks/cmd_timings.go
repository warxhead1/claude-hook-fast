package main

import (
	"fmt"
	"sort"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/timing"
)

// cmdTimings prints a per-(bin,cmd) latency summary from the recorded
// invocation ledger. Sorted by p95 descending — the hooks eating the
// tool-call critical path float to the top.
//
//	claude-hook-fast timings
func cmdTimings() {
	entries := timing.Load(timing.Dir())
	if len(entries) == 0 {
		fmt.Printf("no timings recorded yet in %s\n", timing.Dir())
		return
	}
	type key struct{ bin, cmd string }
	groups := map[key][]float64{}
	last := map[key]string{}
	for _, e := range entries {
		k := key{e.Bin, e.Cmd}
		groups[k] = append(groups[k], e.MS)
		if e.TS > last[k] {
			last[k] = e.TS
		}
	}
	type row struct {
		bin, cmd      string
		count         int
		p50, p95, max float64
		last          string
	}
	var rows []row
	for k, ms := range groups {
		sort.Float64s(ms)
		rows = append(rows, row{
			bin:   k.bin,
			cmd:   k.cmd,
			count: len(ms),
			p50:   ms[len(ms)/2],
			p95:   ms[(len(ms)*95)/100],
			max:   ms[len(ms)-1],
			last:  last[k],
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].p95 > rows[j].p95 })
	fmt.Printf("%-18s %-28s %7s %9s %9s %9s  %s\n",
		"BIN", "CMD", "COUNT", "P50ms", "P95ms", "MAXms", "LAST")
	for _, r := range rows {
		fmt.Printf("%-18s %-28s %7d %9.1f %9.1f %9.1f  %s\n",
			r.bin, r.cmd, r.count, r.p50, r.p95, r.max, r.last)
	}
}
