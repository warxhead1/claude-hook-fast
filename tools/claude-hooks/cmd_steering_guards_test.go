package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDispatchModelGuardMessageMatchesTiering(t *testing.T) {
	msg := agentBlockMsg("map the repo")
	for _, want := range []string{`"haiku"`, `"sonnet"`, `"opus"`, "Fable", "mechanical", "testable", "design that already exists", "judgment"} {
		if !strings.Contains(msg, want) {
			t.Errorf("agent message missing %q:\n%s", want, msg)
		}
	}
	for _, banned := range []string{"minimum for any dispatched agent", "only for the lead session", "inherit session Opus"} {
		if strings.Contains(msg, banned) {
			t.Errorf("agent message still carries contradicted text %q", banned)
		}
	}
	if w := workflowBlockMsg(3, 1); strings.Contains(w, "Opus model") || !strings.Contains(w, "'haiku'") {
		t.Errorf("workflow message not updated:\n%s", w)
	}
}

func TestHostedRunnerLabel(t *testing.T) {
	block := []string{
		"jobs:\n  b:\n    runs-on: ubuntu-latest\n",
		"    runs-on: windows-latest",
		"    runs-on: macos-14",
		"    runs-on: [ubuntu-latest]",
		"    runs-on: \"ubuntu-22.04\"",
		"    runs-on: fancy-latest",
		"  - runs-on: ubuntu-latest # note",
	}
	for _, c := range block {
		if hostedRunnerLabel(c) == "" {
			t.Errorf("must block: %q", c)
		}
	}
	pass := []string{
		"    runs-on: self-hosted",
		"    runs-on: [self-hosted, linux]",
		"    runs-on: [self-hosted, ubuntu-latest]",
		"    runs-on: ${{ matrix.os }}",
		"    # runs-on: ubuntu-latest",
		"    name: ubuntu-latest smoke",
		"    runs-on: self-hosted # not ubuntu-latest",
	}
	for _, c := range pass {
		if l := hostedRunnerLabel(c); l != "" {
			t.Errorf("must pass: %q (got %q)", c, l)
		}
	}
}

func TestHostedRunnerPayloadAndPath(t *testing.T) {
	wf := "/r/.github/workflows/ci.yml"
	cases := []struct {
		tool, in string
		want     int
	}{
		{"Write", `{"file_path":"` + wf + `","content":"runs-on: ubuntu-latest"}`, 1},
		{"Edit", `{"file_path":"` + wf + `","old_string":"a","new_string":"runs-on: ubuntu-latest"}`, 1},
		{"MultiEdit", `{"file_path":"` + wf + `","edits":[{"new_string":"x"},{"new_string":"runs-on: macos-14"}]}`, 2},
	}
	for _, c := range cases {
		path, texts := hostedRunnerPayload(c.tool, json.RawMessage(c.in))
		if path != wf || len(texts) != c.want {
			t.Errorf("%s: path=%q texts=%v", c.tool, path, texts)
		}
	}
	for p, want := range map[string]bool{
		wf: true, ".github/workflows/x.yaml": true,
		"/r/.github/workflows/README.md": false, "/r/docs/ci.yml": false, "/r/.github/dependabot.yml": false,
	} {
		if isWorkflowPath(p) != want {
			t.Errorf("isWorkflowPath(%q) != %v", p, want)
		}
	}
}

// globalRule loads one rule's compiled regex from the live user-global config.
func globalRule(t *testing.T, id string) *regexp.Regexp {
	t.Helper()
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "hook-rules.json"))
	if err != nil {
		t.Skip("no user-global hook-rules.json")
	}
	var cfg struct {
		Rules []struct {
			ID    string `json:"id"`
			Match struct {
				CommandRegex string `json:"command_regex"`
			} `json:"match"`
			Action struct {
				Message string `json:"message"`
			} `json:"action"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, r := range cfg.Rules {
		if r.ID == id {
			return regexp.MustCompile(r.Match.CommandRegex)
		}
	}
	t.Skipf("rule %s not in user-global config", id)
	return nil
}

func TestGlobalSteeringRules(t *testing.T) {
	stash := globalRule(t, "git-stash-bare")
	for _, c := range []string{"git stash", "git -C /x/y stash", "git stash push -u", "cd a && git -C /x stash push"} {
		if !stash.MatchString(stripQuotedSpans(c)) {
			t.Errorf("git-stash-bare must block %q", c)
		}
	}
	for _, c := range []string{"git stash list", "git -C /x stash list", "git stash show -p", "git stash drop stash@{0}", "git status", "git -C /x commit -m 'WIP: stash'"} {
		if stash.MatchString(stripQuotedSpans(c)) {
			t.Errorf("git-stash-bare must pass %q", c)
		}
	}
	pop := globalRule(t, "git-stash-pop-or-bare-apply")
	for _, c := range []string{"git stash pop", "git -C /x stash pop", "git -C /x stash apply"} {
		if !pop.MatchString(stripQuotedSpans(c)) {
			t.Errorf("pop rule must block %q", c)
		}
	}
	for _, c := range []string{"git stash apply abc123", "git -C /x stash apply abc123", "git stash list"} {
		if pop.MatchString(stripQuotedSpans(c)) {
			t.Errorf("pop rule must pass %q", c)
		}
	}
	clean := globalRule(t, "go-clean-cache")
	for _, c := range []string{"go clean -cache", "go clean -modcache -cache", "GOFLAGS=x go clean -cache", "cd x && go clean -cache"} {
		if !clean.MatchString(stripQuotedSpans(c)) {
			t.Errorf("go-clean-cache must block %q", c)
		}
	}
	for _, c := range []string{"go clean", "go clean -testcache", "go clean ./...", "go build ./...", "echo go clean -cache"} {
		if clean.MatchString(stripQuotedSpans(c)) {
			t.Errorf("go-clean-cache must pass %q", c)
		}
	}
}
