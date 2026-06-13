package main

import (
	"flag"
	"os"
)

// parseFlagsForTest reinitializes the global flag set so each test
// gets a fresh flag.Args() / flag.NArg() derived from the current
// os.Args[1:]. The production binary parses flags exactly once; tests
// need to re-parse between cases.
func parseFlagsForTest() {
	flag.CommandLine = flag.NewFlagSet("claude-hook-fast", flag.ContinueOnError)
	if len(os.Args) > 1 {
		_ = flag.CommandLine.Parse(os.Args[1:])
	}
}
