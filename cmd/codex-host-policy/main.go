// codex-host-policy installs the versioned host-wide model and delegation
// contract without replacing unrelated Codex configuration or AGENTS content.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/codexpolicy"
)

const (
	managedBegin = "<!-- BEGIN CLAUDE-HOOK-FAST CODEX HOST POLICY -->"
	managedEnd   = "<!-- END CLAUDE-HOOK-FAST CODEX HOST POLICY -->"
)

type options struct {
	codexHome  string
	configHome string
	hook       string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "check") {
		return errors.New("usage: codex-host-policy <install|check> [--codex-home DIR] [--config-home DIR] [--hook PATH]")
	}
	action := args[0]
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	defaultConfigHome := os.Getenv("XDG_CONFIG_HOME")
	if defaultConfigHome == "" {
		defaultConfigHome = filepath.Join(home, ".config")
	}
	opt := options{}
	fs.StringVar(&opt.codexHome, "codex-home", filepath.Join(home, ".codex"), "Codex configuration directory")
	fs.StringVar(&opt.configHome, "config-home", defaultConfigHome, "XDG-style configuration directory")
	fs.StringVar(&opt.hook, "hook", defaultHookPath(), "installed codex-hook path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return reconcile(action == "install", opt)
}

func defaultHookPath() string {
	path, err := exec.LookPath("codex-hook")
	if err != nil {
		return "codex-hook"
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

func reconcile(write bool, opt options) error {
	policy, err := codexpolicy.Parse(codexpolicy.DefaultPolicyBytes())
	if err != nil {
		return fmt.Errorf("embedded worker policy: %w", err)
	}
	targets := []struct {
		path   string
		render func([]byte) ([]byte, error)
		mode   os.FileMode
	}{
		{
			path: filepath.Join(opt.codexHome, "config.toml"),
			render: func(current []byte) ([]byte, error) {
				return renderConfig(current, policy.DefaultModel, opt.hook)
			},
			mode: 0o600,
		},
		{
			path: filepath.Join(opt.codexHome, "AGENTS.md"),
			render: func(current []byte) ([]byte, error) {
				return renderAgents(current, codexpolicy.ManagedAgentsBytes())
			},
			mode: 0o644,
		},
		{
			path: filepath.Join(opt.configHome, filepath.FromSlash(codexpolicy.PolicyRelPath)),
			render: func([]byte) ([]byte, error) {
				return append(bytes.TrimSpace(codexpolicy.DefaultPolicyBytes()), '\n'), nil
			},
			mode: 0o644,
		},
	}

	drift := false
	for _, target := range targets {
		current, err := os.ReadFile(target.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		desired, err := target.render(current)
		if err != nil {
			return fmt.Errorf("%s: %w", target.path, err)
		}
		if bytes.Equal(current, desired) {
			fmt.Printf("ok %s\n", target.path)
			continue
		}
		drift = true
		if !write {
			fmt.Printf("drift %s\n", target.path)
			continue
		}
		if err := atomicReplace(target.path, desired, target.mode); err != nil {
			return err
		}
		fmt.Printf("installed %s\n", target.path)
	}
	if drift && !write {
		return errors.New("Codex host policy drift detected; run codex-host-policy install")
	}
	return nil
}

var topLevelModelRE = regexp.MustCompile(`^\s*model\s*=`)

func renderConfig(current []byte, model, hookPath string) ([]byte, error) {
	text := strings.ReplaceAll(string(current), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	foundModel := false
	inTopLevel := true
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inTopLevel = false
		}
		if inTopLevel && topLevelModelRE.MatchString(line) {
			if foundModel {
				lines[i] = ""
				continue
			}
			lines[i] = fmt.Sprintf("model = %q", model)
			foundModel = true
		}
	}
	text = strings.Join(lines, "\n")
	if !foundModel {
		text = fmt.Sprintf("model = %q\n", model) + text
	}
	if !hasEnforcingPreToolHook(text) {
		text = strings.TrimRight(text, "\n") + fmt.Sprintf(`

[[hooks.PreToolUse]]
matcher = "Bash"

[[hooks.PreToolUse.hooks]]
type = "command"
command = %q
timeout = 2
`, strings.TrimSpace(hookPath)+" pre-tool")
	}
	return []byte(strings.TrimRight(text, "\n") + "\n"), nil
}

func hasEnforcingPreToolHook(text string) bool {
	groups := strings.Split(text, "[[hooks.PreToolUse]]")
	for _, group := range groups[1:] {
		if next := strings.Index(group, "[[hooks.PreToolUse]]"); next >= 0 {
			group = group[:next]
		}
		matcherHasBash := false
		for _, line := range strings.Split(group, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "matcher") && strings.Contains(trimmed, "Bash") {
				matcherHasBash = true
			}
		}
		if matcherHasBash && strings.Contains(group, "codex-hook pre-tool") {
			return true
		}
	}
	return false
}

func renderAgents(current, policy []byte) ([]byte, error) {
	text := strings.TrimSpace(string(current))
	block := managedBegin + "\n" + strings.TrimSpace(string(policy)) + "\n" + managedEnd
	begin := strings.Index(text, managedBegin)
	end := strings.Index(text, managedEnd)
	switch {
	case begin >= 0 && end >= begin:
		end += len(managedEnd)
		text = strings.TrimSpace(text[:begin] + block + text[end:])
	case begin >= 0 || end >= 0:
		return nil, errors.New("incomplete managed policy markers")
	case strings.HasPrefix(text, "# Global model and delegation contract"):
		// Migrate the unmarked policy installed before this versioned owner existed.
		text = block
	case text == "":
		text = block
	default:
		text = text + "\n\n" + block
	}
	return []byte(text + "\n"), nil
}

func atomicReplace(path string, data []byte, fallbackMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := fallbackMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".bak-codex-host-policy", old, mode); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-host-policy-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
