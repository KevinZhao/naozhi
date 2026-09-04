package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// TranscriptReader streams a subagent's on-disk jsonl transcript and maps
// each line to EventEntry values (docs/rfc agent-team-ui §3.4).
//
// One instance per (key, task_id, jsonl_path); Read/Tail are not
// goroutine-safe with each other. It holds a persistent *os.File (agent_tailer
// polls at 200 ms × up to 50 tailers, mostly reading zero bytes) and reopens
// only when the inode changes. Callers SHOULD Close when done so the fd is
// released eagerly.
type TranscriptReader struct {
	path string

	mu     sync.Mutex
	offset int64
	tail   []byte // half-written trailing line from previous Read
	// readBuf is reused across polls so io.ReadAll's growth chain does not
	// allocate every 200 ms.
	readBuf []byte
	// f is the persistent fd (nil before first read / after Close); statSig
	// identifies its inode so a rotation triggers a reopen via os.SameFile.
	f         *os.File
	statSig   os.FileInfo
	closeOnce sync.Once
}

// NewTranscriptReader constructs a reader anchored at path. path is trusted:
// callers must already have validated it lives under ~/.claude/projects and
// matches agent-<hex>.jsonl.
func NewTranscriptReader(path string) *TranscriptReader {
	return &TranscriptReader{path: path}
}

// Close releases the persistent transcript fd. Idempotent; subsequent
// Read/Tail calls reopen on demand.
func (r *TranscriptReader) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.f != nil {
			err = r.f.Close()
			r.f = nil
			r.statSig = nil
		}
	})
	return err
}

// openOrReuse returns the cached fd, or opens fresh. reset=true means a
// prior fd existed and the inode swapped (rotation), so the caller must drop
// offset/tail; the first open returns reset=false. On Stat/Open errors any
// prior fd is closed. A live fd is returned WITHOUT an os.Stat — an actively
// growing file cannot have rotated; readLocked re-probes only on a zero-byte
// poll (#1884). Caller MUST hold r.mu.
func (r *TranscriptReader) openOrReuse() (*os.File, bool, error) {
	if r.f != nil && r.statSig != nil {
		return r.f, false, nil
	}
	st, err := os.Stat(r.path)
	if err != nil {
		// Path gone: drop the cached fd; surface err verbatim so callers can
		// branch on os.IsNotExist.
		if r.f != nil {
			_ = r.f.Close()
			r.f = nil
			r.statSig = nil
		}
		return nil, false, err
	}
	if r.f != nil && r.statSig != nil && os.SameFile(r.statSig, st) {
		return r.f, false, nil
	}
	hadPrior := r.f != nil
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
		r.statSig = nil
	}
	f, err := os.Open(r.path)
	if err != nil {
		return nil, false, err
	}
	// Re-stat the opened fd: a rotation could swap the file between Stat and Open.
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, false, err
	}
	r.f = f
	r.statSig = fi
	return r.f, hadPrior, nil
}

// Read returns up to `limit` EventEntry values with Time > afterMS. Entries
// with Time == 0 (unparseable timestamp) pass through so the dashboard shows
// something. The filter applies after mapping because one jsonl line yields
// 0..N entries.
func (r *TranscriptReader) Read(afterMS int64, limit int) ([]clievent.EventEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(afterMS, limit)
}

// Tail reads any content written since the last Read/Tail call, returning
// entries in chronological order. Equivalent to Read(lastSeenMS, -1) but
// skips the time filter — tailer callers already know the previous watermark.
func (r *TranscriptReader) Tail() ([]clievent.EventEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(0, 0)
}

func (r *TranscriptReader) readLocked(afterMS int64, limit int) ([]clievent.EventEntry, error) {
	f, reset, err := r.openOrReuse()
	if err != nil {
		return nil, err
	}
	if reset {
		r.offset = 0
		r.tail = nil
	}

	freshBytes, err := r.readFresh(f)
	if err != nil {
		return nil, err
	}
	// A zero-byte poll is the only window in which a rotation can hide (a
	// growing file is provably the same inode), so probe for an inode swap
	// there and re-read once so rotation is caught on this poll (#1884).
	if len(freshBytes) == 0 {
		rotated, perr := r.reprobeRotation()
		if perr != nil {
			return nil, perr
		}
		if rotated {
			r.offset = 0
			r.tail = nil
			freshBytes, err = r.readFresh(r.f)
			if err != nil {
				return nil, err
			}
		}
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
		out      []clievent.EventEntry
		consumed int
	)
	for consumed < len(data) {
		nl := bytes.IndexByte(data[consumed:], '\n')
		if nl < 0 {
			// Partial trailing line: copy so readBuf reuse cannot mutate it.
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
				r.offset = advanceOffset(r.offset, readLen, consumed, data, freshBytes, len(r.tail))
				return out, nil
			}
		}
	}
	// Bytes held in r.tail count as read from the OS, so offset advances fully.
	r.offset += readLen
	return out, nil
}

// readFresh seeks f to r.offset and reads everything available into the
// reused r.readBuf. r.offset points past the bytes already buffered in
// r.tail (the OS has handed them out), so they are never read twice. Caller
// must hold r.mu.
func (r *TranscriptReader) readFresh(f *os.File) ([]byte, error) {
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return nil, err
	}
	// Bound one read so a huge transcript (or symlink swap) cannot pin tens
	// of MB on the polling path; typical files are a few hundred KB.
	const maxTranscriptReadBytes = 16 * 1024 * 1024
	// Retain readBuf capacity across polls unless a one-off oversized poll
	// would pin memory.
	const readBufRetainCap = 256 * 1024
	r.readBuf = r.readBuf[:0]
	freshBytes, err := readAllInto(io.LimitReader(f, maxTranscriptReadBytes), r.readBuf)
	if err != nil {
		return nil, err
	}
	r.readBuf = freshBytes
	if cap(r.readBuf) > readBufRetainCap {
		r.readBuf = nil
	}
	return freshBytes, nil
}

// reprobeRotation runs the Stat + SameFile rotation guard after a zero-byte
// poll. Returns rotated=true with r.f/r.statSig swapped to the new inode, or
// false when unchanged. A vanished path drops the cached fd and surfaces the
// os.IsNotExist error so agent_tailer keeps its 404 semantics. Caller must
// hold r.mu.
func (r *TranscriptReader) reprobeRotation() (bool, error) {
	st, err := os.Stat(r.path)
	if err != nil {
		if r.f != nil {
			_ = r.f.Close()
			r.f = nil
			r.statSig = nil
		}
		return false, err
	}
	if r.f != nil && r.statSig != nil && os.SameFile(r.statSig, st) {
		return false, nil
	}
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
		r.statSig = nil
	}
	f, err := os.Open(r.path)
	if err != nil {
		return false, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return false, err
	}
	r.f = f
	r.statSig = fi
	return true, nil
}

// readAllInto reads everything from r into the supplied buffer, growing it
// via append. Mirrors io.ReadAll (read until EOF, nil err on success) but
// lets the caller reuse a backing slice across polls.
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
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
	}
}

// advanceOffset adjusts r.offset after an early `break` on limit: only the
// fresh bytes fully consumed advance the offset; the remainder is re-read on
// the next call rather than re-buffered into r.tail, keeping the invariant
// that r.offset + len(r.tail) is the next byte the OS has yet to hand us.
func advanceOffset(prev int64, readLen int64, consumed int, data, fresh []byte, tailLen int) int64 {
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
func mapJSONLLine(line []byte) []clievent.EventEntry {
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
		return []clievent.EventEntry{{Time: ts, Type: "system", Summary: "api_error"}}
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
// Content stays RawMessage so flattenToolResultRaw decodes the polymorphic
// shape (string vs array) without interface boxing.
type transcriptUserBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"`
}

// mapUserLine handles content as either plain string (teammate control channel
// or plain prompt) or array of blocks (typically [{"tool_result": ...}]).
func mapUserLine(raw transcriptLine, ts int64) []clievent.EventEntry {
	if raw.Message == nil || len(raw.Message.Content) == 0 {
		return nil
	}

	// String form.
	var s string
	if err := json.Unmarshal(raw.Message.Content, &s); err == nil {
		// teammate-message is the prompt/shutdown control-channel wrapper
		// (user-role only, so substring detection is safe).
		if strings.Contains(s, "<teammate-message teammate_id=") {
			return nil
		}
		return []clievent.EventEntry{{
			Time:    ts,
			Type:    "text",
			Summary: textutil.TruncateRunes(s, 120),
			Detail:  textutil.TruncateRunes(s, EventDetailMaxRunes),
		}}
	}

	// Array form; typed decode avoids per-block interface boxing on the hot
	// pollOnce path.
	var blocks []transcriptUserBlock
	if err := json.Unmarshal(raw.Message.Content, &blocks); err != nil {
		return nil
	}

	var out []clievent.EventEntry
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			out = append(out, clievent.EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: textutil.TruncateRunes(block.Text, 120),
				Detail:  textutil.TruncateRunes(block.Text, EventDetailMaxRunes),
			})
		case "tool_result":
			summary, detail, persistedPath, skip := flattenToolResultRaw(block.Content)
			if skip {
				continue
			}
			entry := clievent.EventEntry{
				Time:    ts,
				Type:    "tool_result",
				Summary: summary,
				Detail:  detail,
			}
			if persistedPath != "" {
				// Tool doubles as the persisted_path carrier; the prefix
				// distinguishes it from real tool names.
				entry.Tool = "persisted:" + persistedPath
			}
			out = append(out, entry)
		}
	}
	return out
}

// transcriptAssistantBlock keeps tool_use input as RawMessage so it goes
// straight to FormatToolInput without a Marshal round-trip.
type transcriptAssistantBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func mapAssistantLine(raw transcriptLine, ts int64) []clievent.EventEntry {
	if raw.Message == nil || len(raw.Message.Content) == 0 {
		return nil
	}
	var blocks []transcriptAssistantBlock
	if err := json.Unmarshal(raw.Message.Content, &blocks); err != nil {
		return nil
	}
	var out []clievent.EventEntry
	for _, block := range blocks {
		switch block.Type {
		case "thinking":
			out = append(out, clievent.EventEntry{
				Time:    ts,
				Type:    "thinking",
				Summary: textutil.TruncateRunes(block.Text, 120),
				Detail:  textutil.TruncateRunes(block.Text, EventDetailMaxRunes),
			})
		case "text":
			out = append(out, clievent.EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: textutil.TruncateRunes(block.Text, 120),
				Detail:  textutil.TruncateRunes(block.Text, EventDetailMaxRunes),
			})
		case "tool_use":
			entry := clievent.EventEntry{
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
			// Agent tool_use inside an agent transcript stays plain tool_use:
			// no nested agent drill-in.
			out = append(out, entry)
		}
	}
	return out
}

// toolResultArrayItem is the typed decode target for the array form of a
// tool_result block's content.
type toolResultArrayItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// flattenToolResultRaw normalises the three shapes of tool_result content
// (string, array, absent) into summary, detail, persistedPath ("" when
// absent), skip. skip=true on decode failure and on a tool_reference-only
// array (pure schema envelope, no UI value).
func flattenToolResultRaw(c json.RawMessage) (string, string, string, bool) {
	if len(c) == 0 {
		return "", "", "", true
	}
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
// persisted-output envelope; the basename is re-prefixed with tool-results/
// for the /api/sessions/tool_result endpoint.
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
	// \r included so CRLF lines from Windows builds still pass the basename regex.
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
