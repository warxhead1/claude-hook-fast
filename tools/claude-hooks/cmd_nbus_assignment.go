// cmd_nbus_assignment.go — PreToolUse emitter for bus.work.assignment.v1.
//
// Fires when an Agent/Task/Workflow dispatch is about to run, so the assignment
// is published when work is handed out, not when the child finishes.
//
// Invariants:
//   - `scope` is parsed only from one strictly valid `scope:` line in the
//     prompt; absent or malformed means JSON null, never a partial scope and
//     never inferred from prose.
//   - The prompt body never leaves this file: only the title, ref ids and the
//     scope globs do.

package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

const chanWorkAssignment = "bus.work.assignment.v1"

const (
	maxScopeGlobs     = 16
	maxScopeGlobBytes = 256
	maxAssignmentRefs = 16
	maxTitleRunes     = 200
)

// scopeBlock is the declared write/read scope of an assignment.
type scopeBlock struct {
	Write    []string `json:"write,omitempty"`
	ReadHint []string `json:"read_hint,omitempty"`
}

var scopeCandidate = regexp.MustCompile("^[\\s`>*-]*scope:")

// scopeGlob is a single comma-list element: no whitespace, separators or '='.
var scopeGlob = regexp.MustCompile(`^[^\s,;=]+$`)

// parseScopeLine reads `scope: write=<glob>[,<glob>...]; read=<glob>[,...]`.
// Either clause may be omitted but not both. Returns false on any deviation.
func parseScopeLine(line string) (*scopeBlock, bool) {
	line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "`"))
	body, ok := strings.CutPrefix(line, "scope:")
	if !ok {
		return nil, false
	}
	var sb scopeBlock
	seen := map[string]bool{}
	for _, clause := range strings.Split(body, ";") {
		clause = strings.TrimSpace(clause)
		key, val, ok := strings.Cut(clause, "=")
		if !ok || (key != "write" && key != "read") || seen[key] {
			return nil, false
		}
		seen[key] = true
		var globs []string
		for _, g := range strings.Split(val, ",") {
			g = strings.TrimSpace(g)
			if !scopeGlob.MatchString(g) || len(g) > maxScopeGlobBytes {
				return nil, false
			}
			globs = append(globs, scopeRefGlob(g))
		}
		if len(globs) == 0 || len(globs) > maxScopeGlobs {
			return nil, false
		}
		if key == "write" {
			sb.Write = globs
		} else {
			sb.ReadHint = globs
		}
	}
	if len(sb.Write) == 0 && len(sb.ReadHint) == 0 {
		return nil, false
	}
	return &sb, true
}

// scopeRefGlob moves an absolute path glob into ref space (`repo:` when the
// literal prefix lies in a git working tree, else `file:`); anything else is
// carried as written.
func scopeRefGlob(g string) string {
	if !filepath.IsAbs(g) {
		return g
	}
	cut := strings.IndexAny(g, "*?[{")
	litDir, tail := g, ""
	if cut >= 0 {
		litDir, tail = g[:cut], g[cut:]
		if i := strings.LastIndexByte(litDir, '/'); i >= 0 {
			litDir, tail = litDir[:i+1], litDir[i+1:]+tail
		}
	}
	f, r := refsForPath(strings.TrimSuffix(litDir, "/"))
	ref := f
	if r != "" {
		ref = r
	}
	if ref == "" {
		return g
	}
	ref = strings.TrimSuffix(ref, "/")
	if tail == "" {
		return ref
	}
	return ref + "/" + tail
}

// extractScope returns the declared scope, or nil. Exactly one scope-intended
// line (a `scope:` line naming write= or read=) must exist and be valid.
func extractScope(prompt string) *scopeBlock {
	var found *scopeBlock
	intended := 0
	for _, line := range strings.Split(prompt, "\n") {
		if !scopeCandidate.MatchString(line) {
			continue
		}
		if !strings.Contains(line, "write=") && !strings.Contains(line, "read=") {
			continue
		}
		intended++
		if sb, ok := parseScopeLine(line); ok {
			found = sb
		}
	}
	if intended != 1 {
		return nil
	}
	return found
}

var (
	beadRefRe  = regexp.MustCompile(`\bbd-[a-z0-9]{2,12}(?:\.[0-9]{1,3})?\b`)
	issueRefRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9._-]{1,63})#([0-9]{1,6})\b`)
	issueURLRe = regexp.MustCompile(`github\.com/[A-Za-z0-9._-]+/([A-Za-z0-9._-]+)/(?:issues|pull)/([0-9]{1,6})\b`)
)

// extractAssignmentRefs finds bead and issue mentions. A bare `#123` carries
// no repo and is ignored.
func extractAssignmentRefs(text string) []string {
	refs := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] && len(refs) < maxAssignmentRefs {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	for _, m := range beadRefRe.FindAllString(text, -1) {
		add("bead:" + m)
	}
	for _, m := range issueURLRe.FindAllStringSubmatch(text, -1) {
		add("issue:" + m[1] + "#" + m[2])
	}
	for _, m := range issueRefRe.FindAllStringSubmatch(text, -1) {
		add("issue:" + m[1] + "#" + m[2])
	}
	return refs
}

type dispatchInput struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

// buildAssignment returns the bus.work.assignment.v1 payload fields for a
// dispatch tool call, or nil when the call has no tool_use_id to key it by.
func buildAssignment(toolName, toolUseID, sessionID string, raw json.RawMessage) map[string]any {
	if toolUseID == "" {
		return nil
	}
	var di dispatchInput
	_ = json.Unmarshal(raw, &di)
	title := strings.TrimSpace(di.Description)
	if title == "" {
		title = di.SubagentType
	}
	if title == "" {
		title = toolName
	}
	var scope any = json.RawMessage("null") // survives the nil-elision in publishNbusEnvelope
	if sb := extractScope(di.Prompt); sb != nil {
		scope = sb
	}
	return map[string]any{
		"assignment_id": toolUseID,
		"worker":        map[string]string{"kind": "claude-subagent", "id": toolUseID},
		"title":         truncate(title, maxTitleRunes),
		"assigned_by":   sessionID,
		"refs":          extractAssignmentRefs(di.Description + "\n" + di.Prompt),
		"scope":         scope,
		"tool_name":     toolName,
		"subagent_type": di.SubagentType,
	}
}

// cmdNbusAssignment is the PreToolUse hook for Agent|Task|Workflow.
func cmdNbusAssignment() {
	in, _ := readStdinForHook()
	switch in.ToolName {
	case "Agent", "Task", "Workflow":
	default:
		return
	}
	fields := buildAssignment(in.ToolName, in.ToolUseID, in.SessionID, in.ToolInput)
	if fields == nil {
		hooklog.Writef("nbus-assignment: %s without tool_use_id; skipped", in.ToolName)
		return
	}
	recordDispatch(in.ToolUseID)
	env, can := baseEnvelope(in, "assigned", in.AgentKind())
	for k, v := range fields {
		env[k] = v
	}
	publishNbusEnvelope(chanWorkAssignment, env, can)
}
