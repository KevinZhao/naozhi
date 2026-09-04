// Package kirojsonl implements history.Source on top of the kiro CLI's
// per-session JSONL transcripts, ~/.kiro/sessions/cli/<sessionId>.jsonl.
//
// Each line is a v1 record with a "kind" tag; only Prompt and
// AssistantMessage are consumed:
//
//	{"version":"v1","kind":"Prompt","data":{"message_id":"...",
//	  "content":[{"kind":"text","data":"..."}],
//	  "meta":{"timestamp":1779081689}}}
//	{"version":"v1","kind":"AssistantMessage","data":{"message_id":"...",
//	  "content":[{"kind":"text","data":"..."}]}}
//
// Other kinds are skipped so the schema can evolve without breaking
// pagination. The session ID comes from a callback re-invoked on every
// LoadBefore so a session/load swap mid-pagination is seen by the next page;
// "" means no kiro session bound yet.
package kirojsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
)

// SessionIDFunc returns the kiro session ID for the bound session, or "" when
// none is negotiated yet. Re-evaluated on every LoadBefore call.
type SessionIDFunc func() string

// maxFileBytes caps how many bytes LoadBefore reads from one session jsonl.
const maxFileBytes = 16 << 20 // 16 MiB

// ctxCheckEvery is how many parsed lines elapse between context.Done checks.
const ctxCheckEvery = 100

// scanBufPool recycles the 64 KiB initial bufio.Scanner buffer across pages.
var scanBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64*1024)
		return &b
	},
}

// maxLineBytes is the longest jsonl record the scan accepts; longer records
// (kiro ToolResults reach tens of MiB) are skipped, not fatal (#2448).
const maxLineBytes = 1 << 20

// lineReaderPool recycles the bufio.Reader between file and scanner that lets
// an oversized record be drained and the scan resumed (see parseFile).
var lineReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

// kindPromptMarker / kindAsstMarker are byte quick-filters so decodeLine can
// skip lines that cannot be a Prompt or AssistantMessage before paying for
// json.Unmarshal. Package scope avoids allocating the literals per line.
var (
	kindPromptMarker = []byte(`"kind":"Prompt"`)
	kindAsstMarker   = []byte(`"kind":"AssistantMessage"`)
)

// Source is the kiro JSONL-backed history.Source.
type Source struct {
	rootDir   string // ~/.kiro/sessions/cli — empty disables the source
	sessionID SessionIDFunc
}

// New constructs a Source. Empty rootDir or nil sessionIDFn yields a
// zero-result Source (LoadBefore returns (nil, nil)) rather than a panic.
func New(rootDir string, sessionIDFn SessionIDFunc) *Source {
	return &Source{rootDir: rootDir, sessionID: sessionIDFn}
}

// init registers the kiro history factory with cli.
func init() {
	cli.RegisterHistoryFactory("kiro", factory)
}

// factory returns cli.NoopHistorySource when the wiring lacks a
// KiroSessionsDir so a router-level misconfig still yields a non-nil source.
func factory(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
	if deps.KiroSessionsDir == "" {
		return cli.NoopHistorySource{}
	}
	return New(deps.KiroSessionsDir, s.SessionID)
}

// kiroRecord is the on-disk wrapper; data stays raw for kind-specific decode.
type kiroRecord struct {
	Version string          `json:"version"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`
}

// kiroContentChunk is one element of a Prompt/AssistantMessage content array.
// Only kind=="text" is rendered; thinking / toolUse / toolResult / image are
// dropped to match the Claude Code chat view. Data is RawMessage because
// non-text chunks carry object payloads — a string field would fail the whole
// content array and drop nearly every real AssistantMessage.
type kiroContentChunk struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// kiroMessageData is the shared shape of Prompt.data and AssistantMessage.data.
// message_id is unused; dedup uses the derived UUID.
type kiroMessageData struct {
	MessageID string             `json:"message_id"`
	Content   []kiroContentChunk `json:"content"`
	Meta      *kiroMessageMeta   `json:"meta,omitempty"`
}

// kiroMessageMeta carries the per-message timestamp. AssistantMessage records
// typically omit meta; see parseFile for how their time is borrowed.
type kiroMessageMeta struct {
	Timestamp int64 `json:"timestamp"` // unix seconds
}

// LoadBefore returns up to `limit` entries strictly older than beforeMS from
// the kiro session's jsonl, oldest → newest; beforeMS <= 0 drops the bound.
// Errors are informational (history.Source treats them as end-of-history).
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	if s == nil || s.rootDir == "" || s.sessionID == nil {
		return nil, nil
	}
	sid := s.sessionID()
	if sid == "" {
		return nil, nil
	}

	// SessionIDFunc is exported and filepath.Join would resolve a traversal
	// sid outside rootDir: reject any sid containing a path separator or "..".
	// A bad sid is "no session", not an error.
	if strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		slog.Warn("kirojsonl: refusing sid containing path separator or '..'",
			"sid_len", len(sid))
		return nil, nil
	}

	path := filepath.Join(s.rootDir, sid+".jsonl")
	f, err := os.Open(path) // #nosec G304 -- sid validated above against path traversal
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("kirojsonl: open %s: %w", path, err)
	}
	defer f.Close()

	// Forward stream with a per-file byte cap; a reverse reader would be
	// cheaper on huge files but is fragile against partial-write tails.
	entries := s.parseFile(ctx, f, beforeMS)

	// parseFile returns chronological order; sort defensively.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Time < entries[j].Time
	})

	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// parseFile streams the jsonl file, decoding each line into an EventEntry that
// satisfies the beforeMS bound. Unknown kinds, blank lines, malformed JSON and
// missing timestamps are skipped individually. Returns arrival order.
//
// Real kiro AssistantMessage records carry no meta.timestamp (only the Prompt
// does), so parseFile remembers the most recent Prompt ts and grants each
// following AssistantMessage that ts plus a monotonic 1 ms offset. Prompt
// timestamps are unix seconds, so the offset never crosses into the next
// prompt.
func (s *Source) parseFile(ctx context.Context, f *os.File, beforeMS int64) []clievent.EventEntry {
	// Read the LAST maxFileBytes: kiro appends chronologically to a single
	// rotation-free file, so reading from 0 would never reach the newest
	// turns. The first line is dropped only when the byte before the window
	// is not '\n' (a genuine half-record); the head backscan then hands back
	// the bytes before the seek point so that record can be reassembled.
	skipPartialFirstLine := false
	// anchor seeds the assistant-timestamp borrow state from the discarded
	// head (#2332), see headPromptAnchor.
	var anchor headAnchor
	if fi, err := f.Stat(); err == nil && fi.Size() > maxFileBytes {
		off := fi.Size() - maxFileBytes
		// Size() > maxFileBytes so off > 0; a non-'\n' byte at off-1 means a partial first line.
		var b [1]byte
		atBoundary := false
		if _, err := f.ReadAt(b[:], off-1); err == nil {
			atBoundary = b[0] == '\n'
		}
		if _, err := f.Seek(off, io.SeekStart); err == nil {
			skipPartialFirstLine = !atBoundary
			anchor = headPromptAnchor(ctx, f, off)
		}
	}
	// br lets an oversized record be drained and a fresh scanner resume without
	// losing buffered bytes (bufio.Scanner is unusable after ErrTooLong) (#2448).
	br := lineReaderPool.Get().(*bufio.Reader)
	br.Reset(io.LimitReader(f, maxFileBytes))
	defer func() {
		br.Reset(nil)
		lineReaderPool.Put(br)
	}()
	// The initial buffer is pooled: bufio.Scanner only grows the slice we hand
	// it, so returning it at zero length recycles the backing array.
	bufPtr := scanBufPool.Get().(*[]byte)
	defer func() {
		b := (*bufPtr)[:0]
		*bufPtr = b
		scanBufPool.Put(bufPtr)
	}()
	scanner := bufio.NewScanner(br)
	scanner.Buffer(*bufPtr, maxLineBytes)

	out := make([]clievent.EventEntry, 0, 16)
	processed := 0
	// Borrow state starts from the head anchor so assistants preceding the
	// first in-window Prompt are kept and their borrowed ts matches a
	// whole-file parse regardless of where the seek point falls.
	lastPromptMS := anchor.promptMS
	asstOffset := anchor.asstOffset
	processLine := func(line []byte) {
		if len(line) == 0 {
			return
		}
		// Byte quick-filter: only Prompt / AssistantMessage decode into an entry (#2246).
		if !bytes.Contains(line, kindPromptMarker) && !bytes.Contains(line, kindAsstMarker) {
			return
		}

		entry, ok := decodeLine(line, lastPromptMS, asstOffset)
		if !ok {
			return
		}
		// A Prompt resets the per-prompt offset; an AssistantMessage that
		// borrowed its ts ticks it. Assistants with their own meta.timestamp
		// don't advance the offset.
		switch entry.Type {
		case "user":
			lastPromptMS = entry.Time
			asstOffset = 0
		case "text":
			if lastPromptMS > 0 && entry.Time == lastPromptMS+1+asstOffset {
				asstOffset++
			}
		}
		if beforeMS > 0 && entry.Time >= beforeMS {
			return
		}
		out = append(out, entry)
	}

	if skipPartialFirstLine {
		if scanner.Scan() {
			// Reassemble the record straddling the seek boundary with the head
			// bytes so it parses (and advances the borrow state) exactly as in
			// a whole-file read; with no fragment it stays dropped.
			if len(anchor.fragment) > 0 {
				processLine(append(anchor.fragment, scanner.Bytes()...))
			}
		} else if errors.Is(scanner.Err(), bufio.ErrTooLong) {
			// The straddling partial line is itself oversized. With an error set,
			// bufio.Scanner would hand the buffered prefix back as a final token
			// and it would reach processLine as a whole record; drain and rebuild.
			if !discardRestOfLine(br) {
				return out
			}
			scanner = bufio.NewScanner(br)
			scanner.Buffer(*bufPtr, maxLineBytes)
		}
	}
	for {
		for scanner.Scan() {
			if processed%ctxCheckEvery == 0 {
				select {
				case <-ctx.Done():
					return out
				default:
				}
			}
			processed++
			processLine(scanner.Bytes())
		}
		err := scanner.Err()
		if !errors.Is(err, bufio.ErrTooLong) {
			if err != nil {
				// Partial-write tails surface here; treat as EOF so accumulated entries are kept.
				slog.Debug("kirojsonl: scanner error treated as EOF", "err", err)
			}
			return out
		}
		// One record exceeds maxLineBytes (#2448): drop the remainder and
		// resume with a fresh scanner so later records still surface. Also
		// covers an oversized partial first line (fragment never reassembled).
		if !discardRestOfLine(br) {
			return out
		}
		scanner = bufio.NewScanner(br)
		scanner.Buffer(*bufPtr, maxLineBytes)
	}
}

// discardRestOfLine drops bytes from br up to and including the next '\n'.
// Returns false when the input ends before a newline is seen.
func discardRestOfLine(br *bufio.Reader) bool {
	for {
		_, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			return true
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return false
		}
	}
}

// headBackscanChunk is the read size for headPromptAnchor's reverse walk.
const headBackscanChunk = 1 << 20 // 1 MiB

// borrowProbeMS is a sentinel lastPromptMS for decodeLine while counting head
// assistants: a record returning borrowProbeMS+1 borrowed its ts (no
// meta.timestamp), the only kind asstOffset counts. Real timestamps are unix
// seconds ×1000 and cannot collide with it.
const borrowProbeMS = 1

// headAnchor is the borrow state recovered from the discarded head of an oversized file.
type headAnchor struct {
	promptMS   int64  // ts (ms) of the most recent complete Prompt before off; 0 = none
	asstOffset int64  // borrowed-ts AssistantMessages between that Prompt and off
	fragment   []byte // head bytes of the record straddling off; nil when none/unknown
}

// headPromptAnchor walks the discarded head [0, off) backwards and returns the
// most recent complete Prompt's timestamp plus the number of borrowed-ts
// assistants between it and off. Real sessions write one Prompt followed by
// MiB of ToolResults, so the tail window often holds no Prompt; without the
// anchor every in-window AssistantMessage would be dropped (#2332). Seeding
// asstOffset with the global count keeps borrowed ts (and UUIDs) identical to
// a whole-file parse. Uses ReadAt so the caller's seek position is untouched;
// cancellation or a read error yields the zero anchor.
func headPromptAnchor(ctx context.Context, f *os.File, off int64) headAnchor {
	var a headAnchor
	// buf is reused across passes: a chunk plus the carry (each ≤1 MiB).
	buf := make([]byte, 0, 2*headBackscanChunk)
	// carry is the tail of a line whose start lies in bytes still to be read.
	var carry []byte
	var count int64
	pos := off
	for pos > 0 {
		select {
		case <-ctx.Done():
			return headAnchor{}
		default:
		}
		n := int64(headBackscanChunk)
		if n > pos {
			n = pos
		}
		chunkStart := pos - n
		buf = buf[:n]
		if _, err := f.ReadAt(buf, chunkStart); err != nil {
			return headAnchor{}
		}
		buf = append(buf, carry...)
		lastNL := bytes.LastIndexByte(buf, '\n')
		if pos == off && (lastNL >= 0 || chunkStart == 0) {
			// Bytes after the last newline are the head of the record straddling
			// the seek point. With no newline in the chunk its start is unknown.
			a.fragment = append([]byte(nil), buf[lastNL+1:]...)
		}
		// Only complete lines are candidates, newest first.
		region := buf[:lastNL+1]
		for len(region) > 0 {
			lineStart := bytes.LastIndexByte(region[:len(region)-1], '\n') + 1
			if lineStart == 0 && chunkStart > 0 {
				// The line begins in an earlier chunk: it becomes the carry.
				break
			}
			line := region[lineStart : len(region)-1]
			region = region[:lineStart]
			// Same quick-filter as the forward scan; decodeLine decides the kind
			// (an AssistantMessage may embed a {"kind":"Prompt"} chunk).
			if !bytes.Contains(line, kindPromptMarker) && !bytes.Contains(line, kindAsstMarker) {
				continue
			}
			e, ok := decodeLine(line, borrowProbeMS, 0)
			if !ok {
				continue
			}
			switch {
			case e.Type == "user":
				a.promptMS = e.Time
				a.asstOffset = count
				return a
			case e.Type == "text" && e.Time == borrowProbeMS+1:
				count++
			}
		}
		if firstNL := bytes.IndexByte(buf, '\n'); firstNL >= 0 {
			carry = append(carry[:0], buf[:firstNL+1]...)
		} else {
			carry = append(carry[:0], buf...)
		}
		if len(carry) > maxLineBytes {
			// Longer than the forward scanner accepts; stop growing the fragment.
			carry = carry[:0]
		}
		pos = chunkStart
	}
	return a
}

// decodeLine parses one jsonl record into an EventEntry; ok is false for
// malformed JSON, unknown kind, missing timestamp or empty content.
// lastPromptMS / asstOffset thread the borrow state so an AssistantMessage
// with no meta.timestamp inherits its Prompt's ts plus a monotonic offset;
// (0, 0) drops orphan assistants.
func decodeLine(line []byte, lastPromptMS, asstOffset int64) (clievent.EventEntry, bool) {
	var rec kiroRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		// Debug, not warn: this is the partial-final-line case during concurrent writes.
		slog.Debug("kirojsonl: skip malformed line", "err", err)
		return clievent.EventEntry{}, false
	}

	var entryType string
	switch rec.Kind {
	case "Prompt":
		entryType = "user"
	case "AssistantMessage":
		// "text" is what dashboard.js renders as a markdown bubble;
		// "assistant" would fall through to the unknown-type card.
		entryType = "text"
	default:
		// Unknown kinds are skipped rather than surfaced as generic system entries.
		return clievent.EventEntry{}, false
	}

	var data kiroMessageData
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		slog.Debug("kirojsonl: skip line with bad data payload", "kind", rec.Kind, "err", err)
		return clievent.EventEntry{}, false
	}

	timeMS, ok := extractTimestampMS(data.Meta)
	if !ok {
		// Borrow the most recent Prompt ts plus offset for an AssistantMessage.
		// Anything else without a ts is dropped rather than forged as ts=0, which
		// would collapse records to epoch and corrupt the strict-< cursor.
		if entryType != "text" || lastPromptMS <= 0 {
			return clievent.EventEntry{}, false
		}
		timeMS = lastPromptMS + 1 + asstOffset
	}

	fullText := concatTextChunks(data.Content)

	// Drop an AssistantMessage with no plain text (thinking + tool_use only)
	// so tool-driven turns don't inject blank cards. Prompts stay permissive
	// so pagination cursors advance.
	if entryType == "text" && strings.TrimSpace(fullText) == "" {
		return clievent.EventEntry{}, false
	}

	// Caps and the deterministic dedup UUID come from the shared recipe (#2336).
	return history.NewDerivedEntry(timeMS, entryType, fullText), true
}

// extractTimestampMS converts a kiro unix-seconds timestamp to milliseconds;
// (0, false) when meta is missing or the timestamp is non-positive.
func extractTimestampMS(meta *kiroMessageMeta) (int64, bool) {
	if meta == nil || meta.Timestamp <= 0 {
		return 0, false
	}
	return meta.Timestamp * 1000, true
}

// concatTextChunks joins all kind=="text" chunks with no separator. Non-text
// chunks have no plain-text representation in the chat view; a text chunk
// whose Data is not a JSON string is skipped.
func concatTextChunks(chunks []kiroContentChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	textChunks := make([]string, 0, len(chunks))
	total := 0
	for _, c := range chunks {
		if c.Kind != "text" {
			continue
		}
		var s string
		if err := json.Unmarshal(c.Data, &s); err != nil {
			continue
		}
		textChunks = append(textChunks, s)
		total += len(s)
	}
	if total == 0 {
		return ""
	}
	if len(textChunks) == 1 {
		return textChunks[0]
	}
	buf := make([]byte, 0, total)
	for _, s := range textChunks {
		buf = append(buf, s...)
	}
	return string(buf)
}
