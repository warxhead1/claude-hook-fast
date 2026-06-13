// Package transcript provides a cheap tail-reader for Claude Code's JSONL
// transcript files.
//
// # Purpose
//
// Claude Code writes per-API-call usage (input_tokens, output_tokens,
// cache_read_input_tokens, cache_creation_input_tokens) and the model ID onto
// the assistant message entry in the JSONL transcript. This information is NOT
// forwarded in the hook stdin payload — it only lives in the transcript.
//
// Hooks that want to forward these signals to the bus must read the transcript.
// A full parse is prohibitively expensive (files grow to 100 k+ lines over a
// session). This package reads the last N bytes of the file and scans backward
// for the most-recent "assistant" entry, which is always the call that triggered
// the current PostToolUse hook.
//
// # Design constraints
//
//   - No heap allocation beyond the tail buffer (fixed size).
//   - No regexp, no heavy JSON decode of the full file.
//   - Graceful zero-return on any I/O or parse error.
//   - Safe to call concurrently (each call opens its own file handle).
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// TailBytes is the number of bytes read from the end of the transcript.
// A typical assistant message (with usage) is 300–2000 bytes. 8 KB gives
// ample headroom for even the largest tool-use response while staying cheap.
const TailBytes = 8192

// Usage carries the token-count fields from the most-recent API call.
// Zero values mean the field was absent or the file could not be read.
type Usage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	Model                    string `json:"model"`
}

// IsZero reports whether all numeric fields are zero (no data available).
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.CacheReadInputTokens == 0 &&
		u.CacheCreationInputTokens == 0
}

// tailReader wraps a ReadSeekCloser so tests can inject a buffer.
type tailReader interface {
	io.ReadSeeker
	io.Closer
}

// LatestUsage reads the last TailBytes of the transcript at path and returns
// the usage block from the most-recent assistant message. Returns a zero Usage
// (and empty model string) on any error.
func LatestUsage(transcriptPath string) Usage {
	if transcriptPath == "" {
		return Usage{}
	}
	f, err := os.Open(transcriptPath) //nolint:gosec // caller-supplied path, not user input
	if err != nil {
		return Usage{}
	}
	defer func() { _ = f.Close() }()
	return latestUsageFromReader(f)
}

// latestUsageFromReader is split out so unit tests can inject bytes.Reader.
func latestUsageFromReader(r tailReader) Usage {
	// Seek to TailBytes before EOF (or 0 if file is smaller).
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return Usage{}
	}
	offset := size - TailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return Usage{}
	}

	buf := make([]byte, size-offset)
	if _, err := io.ReadFull(r, buf); err != nil && err != io.ErrUnexpectedEOF {
		return Usage{}
	}

	return parseLastAssistantUsage(buf)
}

// assistantEntry is the minimal shape we need from a JSONL transcript line.
type assistantEntry struct {
	Type    string `json:"type"`
	Message struct {
		Role  string          `json:"role"`
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
}

// rawUsage mirrors the usage sub-object so we can pull only the fields we care about.
type rawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// parseLastAssistantUsage scans buf for the last complete JSONL line that is
// an assistant message with a non-empty usage block.  Returns zero Usage if
// none is found.
func parseLastAssistantUsage(buf []byte) Usage {
	// Split into lines; iterate in reverse so we find the most recent first.
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		// Quick pre-filter: must contain both "assistant" and "usage" to be
		// a candidate. Avoids full JSON decode on every line.
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}

		var entry assistantEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "assistant" && entry.Message.Role != "assistant" {
			continue
		}
		if len(entry.Message.Usage) == 0 {
			continue
		}

		var ru rawUsage
		if err := json.Unmarshal(entry.Message.Usage, &ru); err != nil {
			continue
		}
		if ru.InputTokens == 0 && ru.OutputTokens == 0 {
			continue
		}

		return Usage{
			InputTokens:              ru.InputTokens,
			OutputTokens:             ru.OutputTokens,
			CacheReadInputTokens:     ru.CacheReadInputTokens,
			CacheCreationInputTokens: ru.CacheCreationInputTokens,
			Model:                    entry.Message.Model,
		}
	}
	return Usage{}
}
