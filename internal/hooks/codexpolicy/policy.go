package codexpolicy

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	PolicyVersion = 1
	PolicyRelPath = "claude-hook-fast/codex-worker-policy.json"
)

//go:embed worker-policy.json AGENTS.md
var assets embed.FS

type WorkerPolicy struct {
	Version                int                         `json:"version"`
	DefaultModel           string                      `json:"default_model"`
	ReadOnlyModel          string                      `json:"read_only_model"`
	AllowedWorkerModels    []string                    `json:"allowed_worker_models"`
	RestrictedWorkerModels map[string]ModelRestriction `json:"restricted_worker_models"`
	RequireExplicitAgent   bool                        `json:"require_explicit_agent"`
	RequireExplicitModel   bool                        `json:"require_explicit_model"`
}

type ModelRestriction struct {
	RequiredEnv   string `json:"required_env"`
	RequiredValue string `json:"required_value"`
}

type Violation struct {
	Reason string
	Model  string
}

func DefaultPolicyBytes() []byte {
	b, err := assets.ReadFile("worker-policy.json")
	if err != nil {
		panic(err)
	}
	return b
}

func ManagedAgentsBytes() []byte {
	b, err := assets.ReadFile("AGENTS.md")
	if err != nil {
		panic(err)
	}
	return b
}

func Parse(data []byte) (WorkerPolicy, error) {
	var p WorkerPolicy
	if err := json.Unmarshal(data, &p); err != nil {
		return WorkerPolicy{}, err
	}
	if p.Version != PolicyVersion {
		return WorkerPolicy{}, fmt.Errorf("unsupported policy version %d", p.Version)
	}
	if strings.TrimSpace(p.DefaultModel) == "" || strings.TrimSpace(p.ReadOnlyModel) == "" {
		return WorkerPolicy{}, errors.New("default_model and read_only_model are required")
	}
	if len(p.AllowedWorkerModels) == 0 {
		return WorkerPolicy{}, errors.New("allowed_worker_models must not be empty")
	}
	allowed := stringSet(p.AllowedWorkerModels)
	if !allowed[p.DefaultModel] || !allowed[p.ReadOnlyModel] {
		return WorkerPolicy{}, errors.New("default and read-only models must be allowed")
	}
	for model, restriction := range p.RestrictedWorkerModels {
		if !allowed[model] {
			return WorkerPolicy{}, fmt.Errorf("restricted model %q must also be allowed", model)
		}
		if strings.TrimSpace(restriction.RequiredEnv) == "" || strings.TrimSpace(restriction.RequiredValue) == "" {
			return WorkerPolicy{}, fmt.Errorf("restricted model %q requires a non-empty env key and value", model)
		}
	}
	return p, nil
}

func Default() WorkerPolicy {
	p, err := Parse(DefaultPolicyBytes())
	if err != nil {
		panic(err)
	}
	return p
}

func PolicyPath() string {
	if override := strings.TrimSpace(os.Getenv("CODEX_WORKER_POLICY_PATH")); override != "" {
		return override
	}
	configDir, err := os.UserConfigDir()
	if err != nil || configDir == "" {
		return ""
	}
	return filepath.Join(configDir, filepath.FromSlash(PolicyRelPath))
}

// Load uses embedded restrictive defaults when the versioned host policy has
// not been installed yet. A present but malformed policy fails closed.
func Load() (WorkerPolicy, error) {
	path := PolicyPath()
	if path == "" {
		return Default(), nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return WorkerPolicy{}, err
	}
	return Parse(b)
}

var (
	workerStartRE = regexp.MustCompile(`(?i)^\s*(?:(?:env|command|sudo)\s+)*(?:[A-Za-z_][A-Za-z0-9_]*=[^\s]+\s+)*(?:["']?[^\s"']*/)?orca(?:-ide)?["']?\s+orchestration\s+worker-start(?:\s|$)`)
)

func InspectCommand(command string, policy WorkerPolicy) []Violation {
	var violations []Violation
	for _, segment := range splitShellCommands(command) {
		segment = strings.TrimSpace(segment)
		if !workerStartRE.MatchString(segment) {
			continue
		}
		agents := flagValues(segment, "agent")
		if len(agents) == 0 {
			if policy.RequireExplicitAgent {
				violations = append(violations, Violation{Reason: "Orca worker launch blocked: pass exactly one explicit --agent; unpinned worker identity can inherit an unsafe default."})
			}
			continue
		}
		if len(agents) != 1 {
			violations = append(violations, Violation{Reason: "Orca worker launch blocked: duplicate --agent flags are ambiguous."})
			continue
		}
		if !strings.EqualFold(agents[0], "codex") {
			continue
		}

		models := flagValues(segment, "model")
		if len(models) == 0 {
			if policy.RequireExplicitModel {
				violations = append(violations, Violation{Reason: fmt.Sprintf("Codex worker launch blocked: pass exactly one explicit --model. Use %s for read-only work.", policy.ReadOnlyModel)})
			}
			continue
		}
		if len(models) != 1 {
			violations = append(violations, Violation{Reason: "Codex worker launch blocked: duplicate --model flags are ambiguous."})
			continue
		}
		model := models[0]
		if !stringSet(policy.AllowedWorkerModels)[model] {
			violations = append(violations, Violation{Model: model, Reason: fmt.Sprintf("Codex worker launch blocked: model %s is not in the host allowlist. Use %s for read-only work or update the versioned policy deliberately.", model, policy.ReadOnlyModel)})
			continue
		}
		if restriction, ok := policy.RestrictedWorkerModels[model]; ok && !hasEnvOptIn(segment, restriction) {
			violations = append(violations, Violation{Model: model, Reason: fmt.Sprintf("Codex worker launch blocked: model %s requires an explicit %s=%s launch prefix after a concrete exceptional reason is stated.", model, restriction.RequiredEnv, restriction.RequiredValue)})
		}
	}
	return violations
}

// splitShellCommands separates the shell control operators relevant to command
// chaining without treating operators inside quoted arguments as boundaries. It
// deliberately does not attempt to execute or fully parse shell syntax.
func splitShellCommands(command string) []string {
	segments := make([]string, 0, 1)
	start := 0
	var quote byte

	split := func(end, next int) {
		segments = append(segments, command[start:end])
		start = next
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != '\'' && c == '\\' {
			if i+1 < len(command) {
				i++ // escaped characters, including a continued newline, are not operators
			}
			continue
		}
		if c == '\'' || c == '"' {
			if quote == 0 {
				quote = c
			} else if quote == c {
				quote = 0
			}
			continue
		}
		if quote != 0 {
			continue
		}

		switch c {
		case ';', '\n':
			split(i, i+1)
		case '\r':
			if i+1 < len(command) && command[i+1] == '\n' {
				split(i, i+2)
				i++
			}
		case '&':
			if i+1 < len(command) && command[i+1] == '&' {
				split(i, i+2)
				i++
			} else {
				split(i, i+1)
			}
		case '|':
			next := i + 1
			if next < len(command) && (command[next] == '|' || command[next] == '&') {
				next++
			}
			split(i, next)
			i = next - 1
		}
	}
	segments = append(segments, command[start:])
	return segments
}

func hasEnvOptIn(command string, restriction ModelRestriction) bool {
	assignment := regexp.MustCompile(`(?:^|\s)` + regexp.QuoteMeta(restriction.RequiredEnv) + `=(?:"` + regexp.QuoteMeta(restriction.RequiredValue) + `"|'` + regexp.QuoteMeta(restriction.RequiredValue) + `'|` + regexp.QuoteMeta(restriction.RequiredValue) + `)(?:\s|$)`)
	return assignment.MatchString(command)
}

func flagValues(command, name string) []string {
	re := regexp.MustCompile(`(?:^|\s)--` + regexp.QuoteMeta(name) + `(?:\s*=\s*|\s+)(?:"([^"]+)"|'([^']+)'|([^\s]+))`)
	matches := re.FindAllStringSubmatch(command, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		for i := 1; i < len(match); i++ {
			if match[i] != "" {
				values = append(values, match[i])
				break
			}
		}
	}
	return values
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
