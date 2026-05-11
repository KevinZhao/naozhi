package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// TranscriptReader streams a subagent's on-disk jsonl transcript (see RFC
// v4 §3.4) and maps each line to an EventEntry using the table in §3.4.1.
//
// Instances are cheap and single-reader: construct once per
// (key, task_id, jsonl_path) tuple. Read/Tail are NOT goroutine-safe with
// each other; callers that want concurrent tail + one-shot fetch should
// serialise via a mutex or use separate TranscriptReader instances.
type TranscriptReader struct {
	path string

	mu     sync.Mutex
	offset int64
	tail   []byte // half-written trailing line from previous Read
}

// NewTranscriptReader constructs a reader anchored at path. path is trusted
// — callers (HTTP handler / server tailer) must have already validated that
// it lives under the ~/.claude/projects tree and passes agent-<hex>.jsonl
// regex (§4 Security).
func NewTranscriptReader(path string) *TranscriptReader {
	return &TranscriptReader{path: path}
}

// Read returns up to `limit` EventEntry values with Time > afterMS. Entries
// with Time == 0 pass through (map_row fills Time from the record's timestamp
// field; if that parse fails Time stays 0 and the entry is still surfaced
// so the dashboard can show something instead of dropping it).
//
// The `afterMS` filter is applied AFTER mapping, not during line scanning,
// because a single jsonl line can collapse into 0 entries (skipped shapes)
// or 1+ entries (assistant with thinking+tool_use+text), and we want stable
// entry-level after-filtering.
func (r *TranscriptReader) Read(afterMS int64, limit int) ([]EventEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(afterMS, limit)
}

// Tail reads any content written since the last Read/Tail call, returning
// entries in chronological order. Equivalent to Read(lastSeenMS, -1) but
// skips the time filter — tailer callers already know the previous watermark.
func (r *TranscriptReader) Tail() ([]EventEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(0, 0)
}

func (r *TranscriptReader) readLocked(afterMS int64, limit int) ([]EventEntry, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Offset semantics: r.offset is the next file byte we haven't yet read
	// as part of a complete line. r.tail is the in-memory buffer of the
	// most recent incomplete trailing line seen on a prior read; its bytes
	// have ALREADY been consumed from the file from the OS's point of view
	// (r.offset points past them), so don't read them twice.
	if r.offset > 0 {
		if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
			return nil, err
		}
	}

	freshBytes, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	readLen := int64(len(freshBytes))

	// Concatenate [prior partial][fresh bytes] for line splitting.
	data := freshBytes
	if len(r.tail) > 0 {
		data = make([]byte, 0, len(r.tail)+len(freshBytes))
		data = append(data, r.tail...)
		data = append(data, freshBytes...)
		r.tail = nil
	}

	var (
		out      []EventEntry
		consumed int
	)
	for consumed < len(data) {
		nl := bytes.IndexByte(data[consumed:], '\n')
		if nl < 0 {
			// Partial trailing line — copy into r.tail (make a fresh slice
			// so subsequent freshBytes reuse doesn't mutate it).
			tail := make([]byte, len(data)-consumed)
			copy(tail, data[consumed:])
			r.tail = tail
			break
		}
		line := data[consumed : consumed+nl]
		consumed += nl + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ents := mapJSONLLine(line)
		for _, e := range ents {
			if afterMS > 0 && e.Time > 0 && e.Time <= afterMS {
				continue
			}
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				// Advance offset past the bytes we actually processed.
				// Since we break early, `consumed` reflects the right boundary.
				r.offset = advanceOffset(r.offset, readLen, consumed, data, freshBytes, len(r.tail))
				return out, nil
			}
		}
	}
	// Advance offset by all fresh bytes consumed as complete lines.
	// Bytes still held in r.tail are fresh-bytes that haven't terminated yet —
	// we count them as "read" from the OS, and remember them in-memory, so
	// offset advances fully.
	r.offset += readLen
	return out, nil
}

// advanceOffset adjusts r.offset after an early `break` on limit. We honor
// the invariant: r.offset + len(r.tail) points at the next byte the OS has
// yet to hand us. When limit truncates processing mid-buffer, bytes between
// `consumed` and the end of `data` are NOT re-buffered into r.tail — they
// have to be re-read on next call, so r.offset stays put and r.tail is
// emptied. This keeps the two bookkeeping cases (normal end vs early return)
// symmetrical and auditable.
func advanceOffset(prev int64, readLen int64, consumed int, data, fresh []byte, tailLen int) int64 {
	// Conservative: on early return, step offset forward only by the amount
	// of `fresh` bytes fully consumed, keeping any remainder for the next
	// Read pass.
	priorBuffered := len(data) - len(fresh) // bytes that came from r.tail
	freshConsumed := consumed - priorBuffered
	if freshConsumed < 0 {
		freshConsumed = 0
	}
	if int64(freshConsumed) > readLen {
		freshConsumed = int(readLen)
	}
	return prev + int64(freshConsumed)
}

// mapJSONLLine transforms one subagent jsonl record into zero or more
// EventEntry values. Malformed lines yield nil (dropped silently so one
// corrupted record does not abort an otherwise-valid transcript).
func mapJSONLLine(line []byte) []EventEntry {
	var raw transcriptLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	ts := parseTranscriptTime(raw.Timestamp)

	switch raw.Type {
	case "user":
		return mapUserLine(raw, ts)
	case "assistant":
		return mapAssistantLine(raw, ts)
	case "system":
		if raw.SubType != "api_error" {
			return nil
		}
		return []EventEntry{{Time: ts, Type: "system", Summary: "api_error"}}
	default:
		return nil
	}
}

type transcriptLine struct {
	Type      string             `json:"type"`
	SubType   string             `json:"subtype"`
	Message   *transcriptMessage `json:"message,omitempty"`
	SessionID string             `json:"sessionId"`
	Timestamp string             `json:"timestamp"`
	PromptID  string             `json:"promptId,omitempty"`
}

type transcriptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// mapUserLine handles content as either plain string (teammate control channel
// or plain prompt) or array of blocks (typically [{"tool_result": ...}]).
func mapUserLine(raw transcriptLine, ts int64) []EventEntry {
	if raw.Message == nil || len(raw.Message.Content) == 0 {
		return nil
	}

	// String form.
	var s string
	if err := json.Unmarshal(raw.Message.Content, &s); err == nil {
		// §3.4.1: teammate-message control channel is the prompt/shutdown
		// packet wrapper. Detect by substring — this shape is user-role only,
		// never assistant, so the false-positive surface is tiny.
		if strings.Contains(s, "<teammate-message teammate_id=") {
			return nil
		}
		return []EventEntry{{
			Time:    ts,
			Type:    "text",
			Summary: TruncateRunes(s, 120),
			Detail:  TruncateRunes(s, 2000),
		}}
	}

	// Array form.
	var arr []map[string]any
	if err := json.Unmarshal(raw.Message.Content, &arr); err != nil {
		return nil
	}

	var out []EventEntry
	for _, block := range arr {
		switch block["type"] {
		case "text":
			txt, _ := block["text"].(string)
			if txt == "" {
				continue
			}
			out = append(out, EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: TruncateRunes(txt, 120),
				Detail:  TruncateRunes(txt, 2000),
			})
		case "tool_result":
			summary, detail, persistedPath, skip := flattenToolResult(block["content"])
			if skip {
				continue
			}
			entry := EventEntry{
				Time:    ts,
				Type:    "tool_result",
				Summary: summary,
				Detail:  detail,
			}
			if persistedPath != "" {
				// Reuse Tool field as the persisted_path carrier so callers
				// (server enrich + dashboard renderer) can special-case it
				// without introducing a new EventEntry field. Prefix distinguishes
				// from real tool names.
				entry.Tool = "persisted:" + persistedPath
			}
			out = append(out, entry)
		}
	}
	return out
}

func mapAssistantLine(raw transcriptLine, ts int64) []EventEntry {
	if raw.Message == nil || len(raw.Message.Content) == 0 {
		return nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw.Message.Content, &blocks); err != nil {
		return nil
	}
	var out []EventEntry
	for _, block := range blocks {
		switch block["type"] {
		case "thinking":
			txt, _ := block["text"].(string)
			out = append(out, EventEntry{
				Time:    ts,
				Type:    "thinking",
				Summary: TruncateRunes(txt, 120),
				Detail:  TruncateRunes(txt, 2000),
			})
		case "text":
			txt, _ := block["text"].(string)
			out = append(out, EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: TruncateRunes(txt, 120),
				Detail:  TruncateRunes(txt, 2000),
			})
		case "tool_use":
			name, _ := block["name"].(string)
			entry := EventEntry{
				Time:    ts,
				Type:    "tool_use",
				Tool:    name,
				Summary: name,
			}
			if input, ok := block["input"]; ok {
				entry.Detail = formatAssistantToolUseDetail(name, input)
			}
			// Per RFC §3.4.1, Agent tool_use inside an agent transcript
			// DOWNGRADES to plain tool_use — we explicitly disable drill-in
			// for this phase (no nested agent views).
			out = append(out, entry)
		}
	}
	return out
}

// flattenToolResult normalises the three observed shapes of tool_result content
// (RFC §3.4.2). Returns summary, detail, persistedPath ("" when absent), skip.
func flattenToolResult(c any) (string, string, string, bool) {
	switch v := c.(type) {
	case string:
		persisted := ""
		if strings.Contains(v, "<persisted-output>") || strings.Contains(v, "saved at:") {
			persisted = extractPersistedPath(v)
		}
		return firstLineTrunc(v, 120), TruncateRunes(v, 16000), persisted, false
	case []any:
		var b strings.Builder
		onlyRefs := true
		for _, item := range v {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			switch m["type"] {
			case "text":
				onlyRefs = false
				if s, _ := m["text"].(string); s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
				}
			case "tool_reference":
				// Drop silently — pure schema envelope.
			}
		}
		if onlyRefs {
			return "", "", "", true
		}
		s := b.String()
		return firstLineTrunc(s, 120), TruncateRunes(s, 16000), "", false
	default:
		return "", "", "", true
	}
}

// persistedPathRe matches the "saved at: <abs path>" line in Claude CLI's
// persisted-output envelope. Captures the absolute path; the basename is
// then re-prefixed with tool-results/ so the client can fetch via the
// /api/sessions/tool_result endpoint (§3.4.2, §3.5.1).
var persistedPathRe = regexp.MustCompile(`saved at:\s*(\S+)`)

// toolResultBasenameRe whitelists persisted-output filenames. CLI today emits
// base36-style ids of length 8-12; we allow up to 32 to tolerate format drift
// and accept .txt/.json/.log extensions only.
var toolResultBasenameRe = regexp.MustCompile(`^[A-Za-z0-9]{1,32}\.(txt|json|log)$`)

func extractPersistedPath(s string) string {
	m := persistedPathRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	abs := m[1]
	// Strip any trailing non-path chars. Includes \r for CRLF-terminated
	// lines the CLI may emit on Windows builds — without it, the basename
	// regex would reject "abc.txt\r" as invalid and drop an otherwise
	// valid persisted-output pointer. R201-SEC-L1.
	abs = strings.TrimRight(abs, ",; \r\n\t")
	idx := strings.LastIndexByte(abs, '/')
	var base string
	if idx < 0 {
		base = abs
	} else {
		base = abs[idx+1:]
	}
	if !toolResultBasenameRe.MatchString(base) {
		return ""
	}
	return "tool-results/" + base
}

func firstLineTrunc(s string, max int) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return TruncateRunes(s, max)
}

func parseTranscriptTime(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// formatAssistantToolUseDetail mirrors internal/cli/process.go's formatToolDetail
// but accepts a raw map[string]any from the transcript rather than a typed
// ContentBlock. Kept lightweight — surfaces common Bash/Read/Edit payloads.
func formatAssistantToolUseDetail(name string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return name
	}
	switch name {
	case "Bash":
		if cmd, _ := m["command"].(string); cmd != "" {
			return "Bash " + TruncateRunes(cmd, 120)
		}
	case "Read":
		if p, _ := m["file_path"].(string); p != "" {
			return "Read " + p
		}
	case "Edit", "Write":
		if p, _ := m["file_path"].(string); p != "" {
			return name + " " + p
		}
	}
	return name
}
