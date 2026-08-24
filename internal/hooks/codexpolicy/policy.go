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
	Version              int      `json:"version"`
	DefaultModel         string   `json:"default_model"`
	ReadOnlyModel        string   `json:"read_only_model"`
	AllowedWorkerModels  []string `json:"allowed_worker_models"`
	BlockedWorkerModels  []string `json:"blocked_worker_models"`
	RequireExplicitAgent bool     `json:"require_explicit_agent"`
	RequireExplicitModel bool     `json:"require_explicit_model"`
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
	blocked := stringSet(p.BlockedWorkerModels)
	if !allowed[p.DefaultModel] || !allowed[p.ReadOnlyModel] {
		return WorkerPolicy{}, errors.New("default and read-only models must be allowed")
	}
	for model := range blocked {
		if allowed[model] {
			return WorkerPolicy{}, fmt.Errorf("model %q is both allowed and blocked", model)
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
	commandSplitRE = regexp.MustCompile(`(?:\r?\n|;|&&|\|\||\|)`)
	workerStartRE  = regexp.MustCompile(`(?i)^\s*(?:(?:env|command|sudo)\s+)*(?:[A-Za-z_][A-Za-z0-9_]*=[^\s]+\s+)*(?:["']?[^\s"']*/)?orca(?:-ide)?["']?\s+orchestration\s+worker-start(?:\s|$)`)
)

func InspectCommand(command string, policy WorkerPolicy) []Violation {
	command = strings.ReplaceAll(command, "\\\n", " ")
	var violations []Violation
	for _, segment := range commandSplitRE.Split(command, -1) {
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
				violations = append(violations, Violation{Reason: "Codex worker launch blocked: pass exactly one explicit --model. Use gpt-5.6-luna for read-only work."})
			}
			continue
		}
		if len(models) != 1 {
			violations = append(violations, Violation{Reason: "Codex worker launch blocked: duplicate --model flags are ambiguous."})
			continue
		}
		model := models[0]
		if stringSet(policy.BlockedWorkerModels)[model] {
			violations = append(violations, Violation{Model: model, Reason: fmt.Sprintf("Codex worker launch blocked: model %s is reserved for the primary coordinator and may not be dispatched as a worker.", model)})
			continue
		}
		if !stringSet(policy.AllowedWorkerModels)[model] {
			violations = append(violations, Violation{Model: model, Reason: fmt.Sprintf("Codex worker launch blocked: model %s is not in the host allowlist. Use %s for read-only work or update the versioned policy deliberately.", model, policy.ReadOnlyModel)})
		}
	}
	return violations
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
