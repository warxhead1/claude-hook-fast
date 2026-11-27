package main

import (
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/warxhead1/claude-hook-fast/internal/hooks/event"
)

// handlePreToolQuestion publishes bus.hearth.session.question.v1 when agy
// invokes its ask_question tool, matching Claude Code's AskUserQuestion
// envelope in tools/claude-hooks/cmd_nbus.go:1006-1100.
func handlePreToolQuestion(payload orderedMap, cwd, proj string) {
	sid := payload.GetString("conversationId")
	if sid == "" {
		return
	}
	corrID := toolCallID(payload)
	if corrID == "" {
		corrID = uuid.New().String()
	}

	type parsedQ struct {
		text    string
		options []string
	}
	var allQuestions []parsedQ
	var verbatimQuestions []any

	args := toolCallArgs(payload)
	if rawQs, ok := args.Get("questions"); ok {
		if qs, ok := rawQs.([]any); ok && len(qs) > 0 {
			for _, qi := range qs {
				qm, ok := qi.(orderedMap)
				if !ok {
					continue
				}
				text := qm.GetString("question")
				var opts []string
				if rawOpts, ok := qm.Get("options"); ok {
					if arr, ok := rawOpts.([]any); ok {
						for _, o := range arr {
							switch v := o.(type) {
							case string:
								if v != "" {
									opts = append(opts, v)
								}
							case orderedMap:
								if label := v.GetString("label"); label != "" {
									opts = append(opts, label)
								}
							}
						}
					}
				}
				if text != "" {
					allQuestions = append(allQuestions, parsedQ{text: text, options: opts})
					qObj := map[string]any{
						"question": text,
						"options":  opts,
					}
					if ms, ok := qm.Get("is_multi_select"); ok {
						if b, ok := ms.(bool); ok {
							qObj["multiSelect"] = b
						}
					}
					verbatimQuestions = append(verbatimQuestions, qObj)
				}
			}
		}
	}
	if len(allQuestions) == 0 {
		allQuestions = append(allQuestions, parsedQ{text: "ask_question"})
		verbatimQuestions = []any{map[string]any{"question": "ask_question", "options": []string{}}}
	}

	env := map[string]any{
		"correlation_id": corrID,
		"session_id":     sid,
		"tool_use_id":    corrID,
		"question":       allQuestions[0].text,
		"options":        allQuestions[0].options,
		"questions_all":  verbatimQuestions,
		"project":        proj,
		"project_short":  filepath.Base(proj),
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
		"pane_id":        event.PaneIDQualified(),
	}
	stampOrcaIdentity(env)
	publishEnvelope(chanSessionQuestion, env, proj)
}

// handlePostToolQuestion publishes bus.hearth.session.question.responded.v1
// when agy's ask_question tool completes so Hearth dismisses the card.
func handlePostToolQuestion(payload orderedMap, proj string) {
	sid := payload.GetString("conversationId")
	if sid == "" {
		return
	}
	env := map[string]any{
		"correlation_id": toolCallID(payload),
		"session_id":     sid,
		"answered_by":    "terminal",
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
	}
	publishEnvelope(chanSessionQuestionResponded, env, proj)
}

// handleSubagentLifecycle publishes bus.hearth.subagent.lifecycle.v1 on
// PreToolUse (started) and PostToolUse (stopped) for invoke_subagent /
// run_workflow, mirroring Claude Code's cmdNbusSubagentLifecycle.
func handleSubagentLifecycle(payload orderedMap, lifecycle, cwd, proj string) {
	sid := payload.GetString("conversationId")
	if sid == "" {
		return
	}
	args := toolCallArgs(payload)
	subType := args.GetString("TypeName")
	if subType == "" {
		subType = args.GetString("Role")
	}
	if subType == "" {
		subType = toolCallMap(payload).GetString("name")
	}
	env, _ := buildEnvelope(payload, "subagent_"+lifecycle, "", "")
	env["subagent_type"] = subType
	env["subagent_id"] = toolCallID(payload)
	publishEnvelope(chanSubagentLifecycle, env, proj)
}
