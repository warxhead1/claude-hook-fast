package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/hooklog"
)

const (
	needsYouSource    = "/antigravity-cli/needs-you"
	needsYouMaxItems  = 20
	needsYouItemRunes = 500
	needsYouSumRunes  = 300
	needsYouSlugMax   = 32
)

var (
	reNeedsYouHeader  = regexp.MustCompile(`(?i)^\s*(?:#+\s*)?\**\s*needs\s+you\s*(?:\*\*)?\s*(?:\([^)]{0,120}\))?\s*(?:\*\*\s*:|:\s*\*\*|:)\s*(.*)$`)
	reNeedsYouBullet  = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+(.*)$`)
	reNeedsYouStretch = regexp.MustCompile(`\{\{([^{}|]*)\|([^{}]*)\}\}`)
	reNeedsYouNothing = regexp.MustCompile(`(?i)^(nothing|none|n/a|—)(\W|$)`)
	reTrailingLink    = regexp.MustCompile(` <hearth://[^\s<>]+>$`)
	reInlineLink      = regexp.MustCompile(`<?(hearth://[^\s<>)\]]+)>?`)
	reDecisionID      = regexp.MustCompile(`\b((?:dec|src)-[A-Za-z0-9][A-Za-z0-9_-]*)`)
	reMultiSpace      = regexp.MustCompile(`\s{2,}`)
	reNonSlug         = regexp.MustCompile(`[^a-z0-9]+`)
	reAgentHexDir     = regexp.MustCompile(`^agent-[0-9a-f]{6,}$`)
)

// needsYouStateEntry mirrors ~/.local/state/needs-you/sessions/<id>.json
// written by nervous-bus/adapters/needs-you/needs_you.py so `needs-you list`
// and Kiln's needs-you-feed read agy sessions identically to Claude Code.
type needsYouStateEntry struct {
	SessionID    string   `json:"session_id"`
	Project      string   `json:"project"`
	CWD          string   `json:"cwd"`
	Items        []string `json:"items"`
	Summary      string   `json:"summary"`
	OrcaTerminal string   `json:"orca_terminal"`
	Name         string   `json:"name"`
	TS           float64  `json:"ts"`
	Acked        bool     `json:"acked"`
}

// attentionPayload matches nervous-bus/schemas/agent.attention.v1.json and
// needs_you.py's publish() output.
type attentionPayload struct {
	SessionID    string   `json:"session_id"`
	Project      string   `json:"project"`
	CWD          string   `json:"cwd"`
	Items        []string `json:"items"`
	Summary      string   `json:"summary"`
	OrcaTerminal string   `json:"orca_terminal"`
	Name         string   `json:"name,omitempty"`
}

func needsYouHome() string {
	if h := os.Getenv("NEEDS_YOU_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".local", "state", "needs-you")
}

func unstretchNeedsYou(text string) string {
	for {
		next := reNeedsYouStretch.ReplaceAllString(text, "$2")
		if next == text {
			return text
		}
		text = next
	}
}

func linkNeedsYouItem(item string) string {
	if reTrailingLink.MatchString(item) {
		return item
	}
	if loc := reInlineLink.FindStringSubmatchIndex(item); loc != nil {
		url := strings.TrimRight(item[loc[2]:loc[3]], ".,;:")
		rest := item[:loc[0]] + item[loc[1]:]
		rest = strings.TrimSpace(reMultiSpace.ReplaceAllString(rest, " "))
		return rest + " <" + url + ">"
	}
	if m := reDecisionID.FindStringSubmatch(item); m != nil {
		id := strings.TrimRight(m[1], "-_")
		return item + " <hearth://decisions/" + id + ">"
	}
	return item
}

// extractNeedsYou extracts items from the LAST "Needs you" block in reply.
// Returns nil when no header is present; returns empty non-nil slice []string{}
// when the block says "nothing" (clearing earlier asks for this session).
func extractNeedsYou(reply string) []string {
	lines := strings.Split(unstretchNeedsYou(reply), "\n")
	start := -1
	for i, line := range lines {
		if reNeedsYouHeader.MatchString(line) {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	m := reNeedsYouHeader.FindStringSubmatch(lines[start])
	rest := strings.TrimSpace(strings.Trim(strings.TrimSpace(m[1]), "*"))
	raw := make([]string, 0)
	if rest != "" && !reNeedsYouNothing.MatchString(rest) {
		raw = append(raw, rest)
	}
	for _, line := range lines[start+1:] {
		if bm := reNeedsYouBullet.FindStringSubmatch(line); bm != nil {
			raw = append(raw, strings.TrimSpace(bm[1]))
		} else if strings.TrimSpace(line) != "" {
			break
		}
	}
	items := make([]string, 0, len(raw))
	for _, it := range raw {
		if it == "" || reNeedsYouNothing.MatchString(it) {
			continue
		}
		items = append(items, truncateRunes(linkNeedsYouItem(it), needsYouItemRunes))
		if len(items) >= needsYouMaxItems {
			break
		}
	}
	return items
}

func summaryOfReply(reply string) string {
	for _, line := range strings.Split(unstretchNeedsYou(reply), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" && !reNeedsYouHeader.MatchString(line) && !reNeedsYouBullet.MatchString(line) {
			return truncateRunes(line, needsYouSumRunes)
		}
	}
	return ""
}

func slugifyName(s string) string {
	s = strings.Trim(reNonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > needsYouSlugMax {
		s = strings.TrimRight(s[:needsYouSlugMax], "-")
	}
	return s
}

func defaultSessionName(cwd string) string {
	if cwd == "" {
		return ""
	}
	clean := filepath.Clean(cwd)
	parts := strings.Split(clean, string(filepath.Separator))
	if len(parts) < 2 {
		return ""
	}
	inWorktreeRoot := false
	for _, p := range parts[:len(parts)-1] {
		if p == "worktrees" || p == "orca" {
			inWorktreeRoot = true
			break
		}
	}
	if !inWorktreeRoot {
		return ""
	}
	base := parts[len(parts)-1]
	if reAgentHexDir.MatchString(base) {
		return ""
	}
	return slugifyName(base)
}

func resolveSessionName(sessionID, cwd string) string {
	if sessionID != "" && !strings.ContainsAny(sessionID, "/.") {
		p := filepath.Join(needsYouHome(), "names", sessionID)
		if b, err := os.ReadFile(p); err == nil {
			if chosen := strings.TrimSpace(string(b)); chosen != "" {
				return chosen
			}
		}
	}
	return defaultSessionName(cwd)
}

type agyTranscriptEntry struct {
	Type      string            `json:"type"`
	Content   string            `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

// agyLastTurnReply reads an Antigravity transcript.jsonl file and returns
// the model's final reply text after the last USER_INPUT (and after the
// last tool call in that turn).
func agyLastTurnReply(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var texts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e agyTranscriptEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "USER_INPUT":
			texts = nil
		case "PLANNER_RESPONSE":
			if len(e.ToolCalls) > 0 {
				texts = nil
			} else if e.Content != "" {
				texts = append(texts, e.Content)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// harvestNeedsYou reads the turn's final reply from agy's transcript,
// extracts any "**Needs you:**" block, writes ~/.local/state/needs-you/sessions/<sid>.json,
// and publishes agent.attention.v1 in the exact shape Claude's needs-you hook uses.
func harvestNeedsYou(payload orderedMap, cwd, proj string) {
	if os.Getenv("NEEDS_YOU_DISABLE") == "1" {
		return
	}
	sid := payload.GetString("conversationId")
	if sid == "" || strings.ContainsAny(sid, "/.") {
		return
	}
	reply := agyLastTurnReply(payload.GetString("transcriptPath"))
	if reply == "" {
		return
	}
	items := extractNeedsYou(reply)
	if items == nil {
		return
	}

	name := resolveSessionName(sid, cwd)
	summary := summaryOfReply(reply)
	orcaTerm := event.OrcaTerminalHandle()
	entry := needsYouStateEntry{
		SessionID:    sid,
		Project:      proj,
		CWD:          cwd,
		Items:        items,
		Summary:      summary,
		OrcaTerminal: orcaTerm,
		Name:         name,
		TS:           float64(time.Now().UnixNano()) / 1e9,
		Acked:        false,
	}

	sessionsDir := filepath.Join(needsYouHome(), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err == nil {
		if raw, err := json.Marshal(entry); err == nil {
			writeAtomicFile(sessionsDir, filepath.Join(sessionsDir, sid+".json"), raw)
		}
	}

	att := attentionPayload{
		SessionID:    sid,
		Project:      proj,
		CWD:          cwd,
		Items:        items,
		Summary:      summary,
		OrcaTerminal: orcaTerm,
		Name:         name,
	}
	raw, err := json.Marshal(att)
	if err != nil {
		hooklog.WriteEvent("agy_hook_attention_marshal_failed", map[string]any{"error": err.Error()})
		return
	}
	if err := publishAsync(chanAgentAttention, needsYouSource, string(raw)); err != nil {
		hooklog.WriteEvent("agy_hook_attention_publish_failed", map[string]any{"error": err.Error()})
	}
}
