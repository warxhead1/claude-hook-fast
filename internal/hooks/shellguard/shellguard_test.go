package shellguard

import "testing"

func TestInspect(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		cwd       string
		wantBlock bool
		wantRule  string
	}{
		{"plain sudo", "sudo rm somefile", "/repo", true, "sudo-any"},
		{"sudo mid-chain", "go build ./... && sudo make install", "/repo", true, "sudo-any"},
		{"sudo in command substitution", "echo $(sudo whoami)", "/repo", true, "sudo-any"},
		{"sudoku is not sudo", "echo sudoku-solver", "/repo", false, ""},
		{"bare git stash", "git stash", "/repo", true, "git-stash-bare"},
		{"git stash push message", "git stash push -m wip", "/repo", true, "git-stash-bare"},
		{"pathspec stash allowed", "git stash push -- foo.go", "/repo", false, ""},
		{"git stash list allowed", "git stash list", "/repo", false, ""},
		{"git stash show allowed", "git stash show -p", "/repo", false, ""},
		{"git stash pop", "git stash pop", "/repo", true, "git-stash-pop-or-bare-apply"},
		{"git stash apply bare", "git stash apply", "/repo", true, "git-stash-pop-or-bare-apply"},
		{"git stash apply with ref allowed", "git stash apply stash@{1}", "/repo", false, ""},
		{"git clean -f", "git clean -f", "/repo", true, "git-clean-force"},
		{"git clean -fd", "git clean -fd", "/repo", true, "git-clean-force"},
		{"git clean dry run allowed", "git clean -n", "/repo", false, ""},
		{"git reset hard origin main", "git reset --hard origin/main", "/repo", true, "git-reset-hard-origin"},
		{"git reset hard upstream", "git reset --hard @{u}", "/repo", true, "git-reset-hard-origin"},
		{"git reset hard local sha allowed", "git reset --hard HEAD~1", "/repo", false, ""},
		{"git reset soft origin allowed", "git reset --soft origin/main", "/repo", false, ""},
		{"chained commit and push clean", "git add -A && git commit -m wip && git push", "/repo", false, ""},
		{"empty command", "", "/repo", false, ""},
		{"rm -rf outside allowlist", "rm -rf /home/eric/data2/worktrees/foo/build", "/home/eric/data2/worktrees/foo", true, "recursive-delete-rm"},
		{"rm -rf under claude-tmp allowed", "rm -rf /home/eric/data2/claude-tmp/scratch", "/repo", false, ""},
		{"rm -rf under tmp allowed", "rm -rf /tmp/scratch-dir", "/repo", false, ""},
		{"rm -rf under evidence allowed", "rm -rf /home/eric/data2/orca/nervous-bus/_evidence/2026-09-25/scratch", "/repo", false, ""},
		{"rm -rf relative resolves against cwd, outside allowlist", "rm -rf build", "/home/eric/projects/foo", true, "recursive-delete-rm"},
		{"rm -rf relative resolves against cwd, inside evidence", "rm -rf scratch", "/home/eric/data2/orca/nervous-bus/_evidence/2026-09-25", false, ""},
		{"rm -rf variable only operand blocked", "rm -rf \"$DIR\"", "/repo", true, "recursive-delete-rm"},
		{"non-recursive rm allowed", "rm somefile.txt", "/repo", false, ""},
		{"find -delete outside allowlist", "find /home/eric/projects/foo -name '*.tmp' -delete", "/repo", true, "recursive-delete-find"},
		{"find -delete under tmp allowed", "find /tmp/scratch -name '*.tmp' -delete", "/repo", false, ""},
		{"git clean via -C outside allowlist", "git -C /home/eric/projects/foo clean -fdx", "/repo", true, "recursive-delete-git-clean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := Inspect(tt.command, tt.cwd)
			if tt.wantBlock && v == nil {
				t.Fatalf("Inspect(%q) = nil, want block (%s)", tt.command, tt.wantRule)
			}
			if !tt.wantBlock && v != nil {
				t.Fatalf("Inspect(%q) = block %+v, want pass", tt.command, v)
			}
			if tt.wantBlock && v.RuleID != tt.wantRule {
				t.Fatalf("Inspect(%q) rule = %q, want %q", tt.command, v.RuleID, tt.wantRule)
			}
			if tt.wantBlock && v.Message == "" {
				t.Fatalf("Inspect(%q) blocked with empty message", tt.command)
			}
		})
	}
}

func TestInspectFailsOpenOnUnparseableSegment(t *testing.T) {
	// Unbalanced quote: shellSplit errors, the segment is skipped, and no
	// other guard fires — fail open, never block on a parse error.
	if v := Inspect(`echo "unterminated`, "/repo"); v != nil {
		t.Fatalf("Inspect on unparseable segment = %+v, want pass (fail open)", v)
	}
}

func TestGuardrailsAssetLoads(t *testing.T) {
	rs := loadRules()
	if len(rs) == 0 {
		t.Fatal("loadRules() returned no rules — guardrails.json embed or parse is broken")
	}
	want := map[string]bool{
		"sudo-any": true, "git-stash-bare": true, "git-stash-pop-or-bare-apply": true,
		"git-clean-force": true, "git-reset-hard-origin": true,
	}
	for _, r := range rs {
		delete(want, r.id)
	}
	if len(want) != 0 {
		t.Fatalf("guardrails.json missing expected rule ids: %v", want)
	}
}
