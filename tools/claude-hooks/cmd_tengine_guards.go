package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

// Ports of:
//   ~/.claude/hooks/post-tool-use/hardcoded-constants-guard.js  (BLOCKING)
//   ~/.claude/hooks/post-tool-use/shader-hardcoded-ids-guard.js (non-blocking, writes to feedback-db)
//
// Both gate only on tengine paths (matches "/tengine/" or "/tengine-"), skip
// generated files, and parse the same Write/Edit/MultiEdit tool_input shape
// used by Claude Code's hook payload.

var tenginePrefixes = []string{"RES_", "WT_", "POOL_", "GIGA_HEALTH_", "POOL_FLAG_", "POOL_MAX_"}

var generatedConstantsFiles = map[string]bool{
	"constants.rs":             true,
	"tengine_constants.h":      true,
	"tengine_constants.glsl":   true,
	"tengine_constants.slang":  true,
	"constants_generator.py":   true,
	"tengine-constants.json":   true,
}

var acceptableAliases = []*regexp.Regexp{
	regexp.MustCompile(`const\s+\w+:\s*usize\s*=\s*(?:tengine_schema_types::generated::constants::|constants::)\w+\s+as\s+usize`),
	regexp.MustCompile(`use\s+tengine_schema_types::generated::constants::`),
	regexp.MustCompile(`pub\s+use\s+tengine_schema_types::generated::constants::`),
	regexp.MustCompile(`#include\s+["<]tengine_constants\.h[">]`),
}

// Precompile per-prefix Rust + C patterns once.
var (
	rustConstPatterns = mustBuildRustPatterns()
	cDefinePatterns   = mustBuildCDefinePatterns()
)

func mustBuildRustPatterns() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(tenginePrefixes))
	for _, p := range tenginePrefixes {
		out[p] = regexp.MustCompile(`const\s+(` + p + `\w+)\s*:\s*(?:u32|usize|u64|i32)\s*=\s*(\d+)\s*;`)
	}
	return out
}

func mustBuildCDefinePatterns() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(tenginePrefixes))
	for _, p := range tenginePrefixes {
		out[p] = regexp.MustCompile(`#define\s+(` + p + `\w+)\s+(\d+)`)
	}
	return out
}

type editExtract struct {
	content  string
	filePath string
}

func extractEditContent(input *hookio.HookInput) *editExtract {
	if input == nil || len(input.ToolInput) == 0 {
		return nil
	}
	var ti map[string]any
	if err := json.Unmarshal(input.ToolInput, &ti); err != nil {
		return nil
	}
	fp, _ := ti["file_path"].(string)
	switch input.ToolName {
	case "Write":
		c, _ := ti["content"].(string)
		return &editExtract{content: c, filePath: fp}
	case "Edit":
		c, _ := ti["new_string"].(string)
		return &editExtract{content: c, filePath: fp}
	case "MultiEdit":
		edits, ok := ti["edits"].([]any)
		if !ok {
			return nil
		}
		var b strings.Builder
		for i, e := range edits {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			s, _ := m["new_string"].(string)
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(s)
		}
		return &editExtract{content: b.String(), filePath: fp}
	}
	return nil
}

func isTengineFile(fp string) bool {
	return strings.Contains(fp, "/tengine/") || strings.Contains(fp, "/tengine-")
}

func isRustOrC(fp string) bool {
	ext := filepath.Ext(fp)
	return ext == ".rs" || ext == ".c" || ext == ".h"
}

type constViolation struct {
	name  string
	value string
	line  string
}

func scanHardcodedConstants(content string) []constViolation {
	var out []constViolation
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") || strings.HasPrefix(line, "*") {
			continue
		}
		skip := false
		for _, re := range acceptableAliases {
			if re.MatchString(line) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if strings.HasPrefix(line, "use ") || strings.HasPrefix(line, "pub use ") {
			continue
		}
		for _, prefix := range tenginePrefixes {
			if m := rustConstPatterns[prefix].FindStringSubmatch(line); m != nil {
				out = append(out, constViolation{name: m[1], value: m[2], line: line})
				break
			}
			if m := cDefinePatterns[prefix].FindStringSubmatch(line); m != nil {
				out = append(out, constViolation{name: m[1], value: m[2], line: line})
				break
			}
		}
	}
	return out
}

func cmdHardcodedConstantsGuard() {
	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}
	ex := extractEditContent(input)
	if ex == nil || ex.filePath == "" {
		return
	}
	if !isTengineFile(ex.filePath) || !isRustOrC(ex.filePath) {
		return
	}
	if generatedConstantsFiles[filepath.Base(ex.filePath)] {
		return
	}

	vios := scanHardcodedConstants(ex.content)
	if len(vios) == 0 {
		return
	}

	var names strings.Builder
	for _, v := range vios {
		fmt.Fprintf(&names, "  %s = %s\n", v.name, v.value)
	}
	msg := fmt.Sprintf(`HARDCODED CONSTANT REDEFINITION DETECTED in %s:

%s
These constants are auto-generated from config/tengine-constants.json.
Use the generated constants instead:

  Rust:  use tengine_schema_types::generated::constants::%s;
  C:     #include "tengine_constants.h"  (provides %s)
  GLSL:  #include "tengine_constants.glsl"

If the constant doesn't exist in the generated output, add "rust" (or "c"/"glsl")
to its "targets" array in config/tengine-constants.json, then rebuild:
  cargo build -p tengine-schema-types

DO NOT define inline constants for RES_*, WT_*, POOL_*, or GIGA_HEALTH_* values.`,
		filepath.Base(ex.filePath), names.String(), vios[0].name, vios[0].name)

	fmt.Fprint(os.Stderr, msg)
	hookExit(2) // BLOCKING — Claude must fix
}

// ---- shader-hardcoded-ids-guard (non-blocking, writes to feedback-db) ----

type shaderPattern struct {
	regex *regexp.Regexp
	name  string
	hint  string
}

var hardcodedShaderPatterns = []shaderPattern{
	{regex: regexp.MustCompile(`giga_get_resource\s*\(\s*\w+\s*,\s*(\d+)`), name: "giga_get_resource", hint: "Use RES_* constant from tengine_constants"},
	{regex: regexp.MustCompile(`signal_emit\s*\(\s*\w+\s*,\s*(\d+)`), name: "signal_emit", hint: "Use WT_* constant from tengine_constants"},
	{regex: regexp.MustCompile(`signal_is_set\s*\(\s*\w+\s*,\s*(\d+)`), name: "signal_is_set", hint: "Use WT_* constant from tengine_constants"},
	{regex: regexp.MustCompile(`giga_get_pool\s*\(\s*\w+\s*,\s*(\d+)`), name: "giga_get_pool", hint: "Use POOL_* constant from tengine_constants"},
}

var shadowConstPattern = regexp.MustCompile(`(?:static\s+)?const\s+uint\s+((?:SLOT|RES|WT|POOL)_\w+)\s*=\s*(\d+)`)

type shaderViolation struct {
	line    string
	lineNum int
	fn      string
	id      string
	hint    string
}

func isShaderFile(fp string) bool {
	ext := strings.ToLower(filepath.Ext(fp))
	return ext == ".slang" || ext == ".glsl"
}

func isGeneratedShader(fp string) bool {
	return strings.Contains(fp, "/slang_generated/") || strings.Contains(fp, "/generated/")
}

func scanShaderHardcodedIDs(content string) []shaderViolation {
	var out []shaderViolation
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "//") {
			continue
		}
		for _, p := range hardcodedShaderPatterns {
			for _, m := range p.regex.FindAllStringSubmatch(line, -1) {
				if p.name == "giga_get_resource" && m[1] == "0" {
					continue
				}
				out = append(out, shaderViolation{line: line, lineNum: i + 1, fn: p.name, id: m[1], hint: p.hint})
			}
		}
		for _, m := range shadowConstPattern.FindAllStringSubmatch(line, -1) {
			out = append(out, shaderViolation{
				line:    line,
				lineNum: i + 1,
				fn:      "const definition",
				id:      m[2],
				hint:    fmt.Sprintf("%s should come from #include tengine_constants, not be redefined", m[1]),
			})
		}
	}
	return out
}

// addFeedbackMessage appends a {source,message,priority,timestamp} row to the
// session-isolated /tmp/claude-feedback-messages-<sid>.json JSON array — the
// same on-disk format ~/.claude/hooks/lib/feedback-db.js writes, so the existing
// final-feedback collector picks it up unchanged.
func addFeedbackMessage(sessionID, source, message, priority string) {
	if sessionID == "" {
		return
	}
	path := fmt.Sprintf("/tmp/claude-feedback-messages-%s.json", sessionID)

	var entries []map[string]any
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &entries)
	}
	entries = append(entries, map[string]any{
		"source":    source,
		"message":   message,
		"priority":  priority,
		"timestamp": time.Now().UnixMilli(),
	})
	if data, err := json.Marshal(entries); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

func cmdShaderHardcodedIDsGuard() {
	defer fmt.Println(`{"continue": true}`)

	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}
	ex := extractEditContent(input)
	if ex == nil || ex.filePath == "" {
		return
	}
	if !isTengineFile(ex.filePath) || !isShaderFile(ex.filePath) || isGeneratedShader(ex.filePath) {
		return
	}

	vios := scanShaderHardcodedIDs(ex.content)
	if len(vios) == 0 {
		return
	}

	filename := filepath.Base(ex.filePath)
	var details strings.Builder
	for i, v := range vios {
		if i > 0 {
			details.WriteByte('\n')
		}
		fmt.Fprintf(&details, "   Line %d: `%s`\n   -> %s", v.lineNum, v.line, v.hint)
	}
	message := fmt.Sprintf(
		"⚠️ **Hardcoded resource/WT ID detected in %s**\n%s\n   Hardcoded IDs drift when tsdl_sync reassigns slots.\n   Run `grep RES_ shaders/slang_generated/tengine_constants.slang` to find the right name.",
		filename, details.String(),
	)
	addFeedbackMessage(input.SessionID, "shader-hardcoded-ids-guard", message, "warning")
}
