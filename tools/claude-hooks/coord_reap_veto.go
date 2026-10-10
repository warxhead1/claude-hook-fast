package main

// coord_reap_veto.go — in-use vetoes for coord-reap: a worktree with a keep
// marker or recent changes is reported and never removed.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	coordReapKeepFile = ".keep-worktree"
	coordReapKeepGit  = "keep-worktree"
	// Recency walk bound: skip .git and build/dependency dirs; past this many
	// entries the tree is vetoed rather than proven idle.
	coordReapWalkMax = 200000
)

var coordReapSkipDirs = map[string]bool{".git": true, "node_modules": true, "target": true,
	".cache": true, "__pycache__": true, ".venv": true}

// coordReapVeto returns why worktree p must not be removed (keep marker, or a
// change within the active window), or "". Live cwd is handled separately
// because it can clear after a worker-stop.
func coordReapVeto(ctx context.Context, p string, active time.Duration, now time.Time) string {
	gd := strings.TrimSpace(mustOut(coordReapGit(ctx, p, "rev-parse", "--absolute-git-dir")))
	markers := []string{filepath.Join(p, coordReapKeepFile)}
	if gd != "" {
		markers = append(markers, filepath.Join(gd, coordReapKeepGit))
	}
	for _, mk := range markers {
		if _, err := os.Lstat(mk); err == nil {
			msg := "keep marker " + mk
			if why, _ := os.ReadFile(mk); strings.TrimSpace(string(why)) != "" {
				msg += ": " + coordTrunc(strings.TrimSpace(string(why)), 120)
			}
			return msg
		}
	}
	if recent, detail := coordReapRecentChange(p, gd, now.Add(-active)); recent {
		return fmt.Sprintf("changed within %s (%s)", active, detail)
	}
	return ""
}

// coordReapRecentChange reports whether anything in the tree (excluding .git
// contents and the index, which our own git status rewrites) or the
// worktree's HEAD/reflog changed after cutoff.
func coordReapRecentChange(p, gitDir string, cutoff time.Time) (bool, string) {
	if gitDir != "" {
		for _, f := range []string{"HEAD", "logs/HEAD"} {
			if st, err := os.Stat(filepath.Join(gitDir, f)); err == nil && st.ModTime().After(cutoff) {
				return true, "git " + f
			}
		}
	}
	var hit string
	n := 0
	_ = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != p && coordReapSkipDirs[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			if d.Name() == ".git" {
				return nil
			}
		}
		if n++; n > coordReapWalkMax {
			hit = "tree larger than walk bound"
			return filepath.SkipAll
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(cutoff) {
			hit = strings.TrimPrefix(strings.TrimPrefix(path, p), "/")
			if hit == "" {
				hit = "."
			}
			return filepath.SkipAll
		}
		return nil
	})
	return hit != "", hit
}
