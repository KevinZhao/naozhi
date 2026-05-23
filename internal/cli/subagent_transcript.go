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

	"github.com/naozhi/naozhi/internal/textutil"
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
	// readBuf is reused across Tail/Read polls so io.ReadAll's
	// growth-doubling alloc chain (4 KiB → 8 KiB → 16 KiB) does not fire
	// every poll. Hot-path agent_tailer at 200 ms × 50 tailers
	// → 250 alloc/s without; reusing the buffer drops that to 0 in
	// steady state. R231-PERF-3 / R232-PERF-3.
	readBuf []byte
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

	// Bound a single read so an unexpectedly large transcript (or a
	// symlink-swap pointing at a huge file) cannot pin tens of MB on a
	// hot polling path. Subagent jsonl files are typically a few hundred
	// KB; 16 MB leaves ample headroom for long-running agents.
	// (R227-CR-4)
	const maxTranscriptReadBytes = 16 * 1024 * 1024
	// R231-PERF-3 / R232-PERF-3: reuse r.readBuf across poll calls to
	// dodge io.ReadAll's growth-doubling allocs. readAllInto appends to
	// r.readBuf[:0]; the cap is retained for next call unless it
	// exceeds readBufRetainCap (one-off oversized poll won't pin memory).
	const readBufRetainCap = 256 * 1024
	r.readBuf = r.readBuf[:0]
	freshBytes, err := readAllInto(io.LimitReader(f, maxTranscriptReadBytes), r.readBuf)
	if err != nil {
		return nil, err
	}
	readLen := int64(len(freshBytes))
	r.readBuf = freshBytes
	if cap(r.readBuf) > readBufRetainCap {
		r.readBuf = nil
	}

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
// readAllInto reads everything from r into the supplied buffer, growing it
// in-place via append. Mirrors io.ReadAll's contract (read until EOF,
// nil err on success) but lets the caller hand in a reusable backing slice
// so steady-state polling does not allocate a new buffer for every call.
// R231-PERF-3 / R232-PERF-3.
func readAllInto(r io.Reader, buf []byte) ([]byte, error) {
	if buf == nil {
		buf = make([]byte, 0, 512)
	}
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if err == io.EOF {
				return buf, nil
			}
			return buf, err
		}
	}
}

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

// transcriptUserBlock mirrors transcriptAssistantBlock for the user role.
// Content stays as RawMessage so flattenToolResult can decode the
// polymorphic shape (string vs []any) lazily — same boxing-avoidance
// motivation as R232-CR-17 / R230B-PERF-4.
type transcriptUserBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
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
			Summary: textutil.TruncateRunes(s, 120),
			Detail:  textutil.TruncateRunes(s, 2000),
		}}
	}

	// Array form. Typed decode avoids the `[]map[string]any` interface
	// boxing per block — pollOnce is hot and replays may carry hundreds
	// of tool_result blocks per assistant turn. R230B-PERF-4.
	var blocks []transcriptUserBlock
	if err := json.Unmarshal(raw.Message.Content, &blocks); err != nil {
		return nil
	}

	var out []EventEntry
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			out = append(out, EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: textutil.TruncateRunes(block.Text, 120),
				Detail:  textutil.TruncateRunes(block.Text, 2000),
			})
		case "tool_result":
			summary, detail, persistedPath, skip := flattenToolResultRaw(block.Content)
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

// transcriptAssistantBlock keeps tool_use input as RawMessage so the on-disk
// replay path can hand it straight to FormatToolInput without the previous
// map→Marshal→Unmarshal round-trip (R232-CR-17). pollOnce is hot enough that
// the saved alloc per tool_use block is worth a typed decode.
type transcriptAssistantBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func mapAssistantLine(raw transcriptLine, ts int64) []EventEntry {
	if raw.Message == nil || len(raw.Message.Content) == 0 {
		return nil
	}
	var blocks []transcriptAssistantBlock
	if err := json.Unmarshal(raw.Message.Content, &blocks); err != nil {
		return nil
	}
	var out []EventEntry
	for _, block := range blocks {
		switch block.Type {
		case "thinking":
			out = append(out, EventEntry{
				Time:    ts,
				Type:    "thinking",
				Summary: textutil.TruncateRunes(block.Text, 120),
				Detail:  textutil.TruncateRunes(block.Text, 2000),
			})
		case "text":
			out = append(out, EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: textutil.TruncateRunes(block.Text, 120),
				Detail:  textutil.TruncateRunes(block.Text, 2000),
			})
		case "tool_use":
			entry := EventEntry{
				Time:    ts,
				Type:    "tool_use",
				Tool:    block.Name,
				Summary: block.Name,
			}
			if len(block.Input) > 0 {
				entry.Detail = FormatToolInput(block.Name, block.Input)
			} else {
				entry.Detail = block.Name
			}
			// Per RFC §3.4.1, Agent tool_use inside an agent transcript
			// DOWNGRADES to plain tool_use — we explicitly disable drill-in
			// for this phase (no nested agent views).
			out = append(out, entry)
		}
	}
	return out
}

// toolResultArrayItem is the typed decode target for the array form of a
// tool_result block's content (RFC §3.4.2). Keeping these as a struct
// avoids the per-item interface boxing the old `[]map[string]any` path
// imposed on every replay frame. R230B-PERF-4.
type toolResultArrayItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// flattenToolResultRaw normalises the three observed shapes of tool_result
// content (RFC §3.4.2): string, array, or absent. Returns summary, detail,
// persistedPath ("" when absent), skip.
//
// The string and []item paths are decoded directly from the RawMessage to
// avoid the previous map[string]any boxing per item. Returns skip=true on
// any decode failure (treated as malformed envelope) and on the
// "tool_reference"-only array case (pure schema envelope, no UI value).
func flattenToolResultRaw(c json.RawMessage) (string, string, string, bool) {
	if len(c) == 0 {
		return "", "", "", true
	}
	// String form: decoded with json.Unmarshal so escape sequences are
	// resolved (the same semantics the old `case string:` arm had via
	// json.Unmarshal into []map[string]any → string-typed elements).
	var s string
	if err := json.Unmarshal(c, &s); err == nil {
		persisted := ""
		if strings.Contains(s, "<persisted-output>") || strings.Contains(s, "saved at:") {
			persisted = extractPersistedPath(s)
		}
		return textutil.TruncateRunes(textutil.FirstLineLiteral(s), 120), textutil.TruncateRunes(s, 16000), persisted, false
	}

	// Array form.
	var items []toolResultArrayItem
	if err := json.Unmarshal(c, &items); err != nil {
		return "", "", "", true
	}
	var b strings.Builder
	onlyRefs := true
	for _, m := range items {
		switch m.Type {
		case "text":
			onlyRefs = false
			if m.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(m.Text)
			}
		case "tool_reference":
			// Drop silently — pure schema envelope.
		}
	}
	if onlyRefs {
		return "", "", "", true
	}
	out := b.String()
	return textutil.TruncateRunes(textutil.FirstLineLiteral(out), 120), textutil.TruncateRunes(out, 16000), "", false
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
