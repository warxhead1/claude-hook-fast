package transcript

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

// bytesReadSeeker wraps bytes.Reader to satisfy tailReader interface.
type bytesReadSeeker struct {
	*bytes.Reader
}

func (b *bytesReadSeeker) Close() error { return nil }

func newReader(content string) tailReader {
	return &bytesReadSeeker{bytes.NewReader([]byte(content))}
}

// buildTranscript creates a multi-line JSONL transcript with the given
// assistant entries interspersed with non-assistant noise lines.
func buildTranscript(entries ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"type":"user","message":{"role":"user","content":"hello"}}` + "\n")
	for i, e := range entries {
		sb.WriteString(e + "\n")
		// Add a hook attachment line after each assistant entry.
		sb.WriteString(fmt.Sprintf(`{"type":"attachment","attachment":{"hookEvent":"PostToolUse","toolUseID":"toolu_%d"}}`, i) + "\n")
	}
	return sb.String()
}

func assistantLine(model string, inputTok, outputTok, cacheRead, cacheCreate int) string {
	return fmt.Sprintf(
		`{"type":"assistant","message":{"role":"assistant","model":%q,"content":[{"type":"tool_use","id":"toolu_x","name":"Bash"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
		model, inputTok, outputTok, cacheRead, cacheCreate,
	)
}

func TestLatestUsage_BasicFields(t *testing.T) {
	src := buildTranscript(
		assistantLine("claude-sonnet-4-6", 100, 50, 20, 30),
	)
	got := latestUsageFromReader(newReader(src))
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", got.Model)
	}
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", got.InputTokens)
	}
	if got.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 20 {
		t.Errorf("CacheReadInputTokens = %d, want 20", got.CacheReadInputTokens)
	}
	if got.CacheCreationInputTokens != 30 {
		t.Errorf("CacheCreationInputTokens = %d, want 30", got.CacheCreationInputTokens)
	}
}

func TestLatestUsage_ReturnsMostRecent(t *testing.T) {
	// Two assistant messages — the second (most recent) should win.
	src := buildTranscript(
		assistantLine("claude-opus-4-8", 200, 80, 0, 500),
		assistantLine("claude-sonnet-4-6", 3, 1, 19013, 0),
	)
	got := latestUsageFromReader(newReader(src))
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want most-recent claude-sonnet-4-6", got.Model)
	}
	if got.CacheCreationInputTokens != 0 {
		t.Errorf("CacheCreationInputTokens = %d, want 0 (most-recent)", got.CacheCreationInputTokens)
	}
	if got.CacheReadInputTokens != 19013 {
		t.Errorf("CacheReadInputTokens = %d, want 19013", got.CacheReadInputTokens)
	}
}

func TestLatestUsage_NoAssistantLine(t *testing.T) {
	src := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n"
	got := latestUsageFromReader(newReader(src))
	if !got.IsZero() {
		t.Errorf("expected zero usage, got %+v", got)
	}
}

func TestLatestUsage_EmptyFile(t *testing.T) {
	got := latestUsageFromReader(newReader(""))
	if !got.IsZero() {
		t.Errorf("expected zero on empty file, got %+v", got)
	}
}

func TestLatestUsage_AssistantWithZeroTokens(t *testing.T) {
	// A line that looks like an assistant message but has zero tokens should be skipped.
	src := buildTranscript(
		`{"type":"assistant","message":{"role":"assistant","model":"x","usage":{"input_tokens":0,"output_tokens":0}}}`,
		assistantLine("claude-sonnet-4-6", 5, 2, 0, 0),
	)
	got := latestUsageFromReader(newReader(src))
	// Should return the second one (first valid non-zero from tail) = claude-sonnet-4-6.
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", got.Model)
	}
}

func TestLatestUsage_MalformedJsonLineSkipped(t *testing.T) {
	// Bad JSON on one line should not prevent finding the prior good line.
	src := buildTranscript(
		assistantLine("claude-sonnet-4-6", 10, 5, 0, 0),
	) + `{"type":"assistant","message":{"role":"assistant","model":"bad","usage":{bad json` + "\n"

	got := latestUsageFromReader(newReader(src))
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6 (malformed line skipped)", got.Model)
	}
}

func TestLatestUsage_LargeFile_OnlyReadsTail(t *testing.T) {
	// Construct a file larger than TailBytes with the "wrong" entry at the start
	// and the "right" entry in the tail. The reader should return the tail entry.
	padding := strings.Repeat(`{"type":"user","message":{"role":"user","content":"x"}}`+"\n", TailBytes/60+10)
	tail := buildTranscript(
		assistantLine("claude-opus-4-8", 999, 999, 0, 0),
	)
	src := padding + tail

	got := latestUsageFromReader(newReader(src))
	if got.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8 from tail", got.Model)
	}
	if got.InputTokens != 999 {
		t.Errorf("InputTokens = %d, want 999", got.InputTokens)
	}
}

func TestLatestUsage_RealTranscriptShape(t *testing.T) {
	// Reproduce the exact shape seen in a live transcript from the investigation.
	line := `{"parentUuid":"abc","isSidechain":false,"message":{"model":"claude-sonnet-4-6","id":"msg_01X","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01AMnZ","name":"Bash","input":{"command":"ls"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"cache_creation_input_tokens":19013,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":19013,"ephemeral_1h_input_tokens":0},"output_tokens":1,"service_tier":"standard","inference_geo":"not_available"}},"requestId":"req_x","type":"assistant","uuid":"u1","timestamp":"2026-06-13T05:34:41.769Z"}`

	got := latestUsageFromReader(newReader(line + "\n"))
	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", got.Model)
	}
	if got.InputTokens != 3 {
		t.Errorf("InputTokens = %d, want 3", got.InputTokens)
	}
	if got.CacheCreationInputTokens != 19013 {
		t.Errorf("CacheCreationInputTokens = %d, want 19013", got.CacheCreationInputTokens)
	}
	if got.OutputTokens != 1 {
		t.Errorf("OutputTokens = %d, want 1", got.OutputTokens)
	}
}

// Ensure LatestUsage returns zero for a missing path without panicking.
func TestLatestUsage_MissingFile(t *testing.T) {
	got := LatestUsage("/nonexistent/path/transcript.jsonl")
	if !got.IsZero() {
		t.Errorf("expected zero for missing file, got %+v", got)
	}
}

// seekableError is a tailReader that always returns errors, for
// exercising the seek-failure path.
type seekableError struct{}

func (e *seekableError) Read(_ []byte) (int, error)         { return 0, io.ErrUnexpectedEOF }
func (e *seekableError) Seek(_ int64, _ int) (int64, error) { return 0, io.ErrUnexpectedEOF }
func (e *seekableError) Close() error                       { return nil }

func TestLatestUsage_SeekError(t *testing.T) {
	got := latestUsageFromReader(&seekableError{})
	if !got.IsZero() {
		t.Errorf("expected zero on seek error, got %+v", got)
	}
}
