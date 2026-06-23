// Package kirojsonl implements history.Source on top of the kiro CLI's
// per-session JSONL transcripts under ~/.kiro/sessions/cli.
//
// Unlike Claude Code, kiro persists exactly one .jsonl per session keyed
// by the session UUID with no project-slug subdirectory:
//
//	~/.kiro/sessions/cli/<sessionId>.jsonl
//
// Each line is a self-describing v1 record with a "kind" tag. Sprint 1c
// only consumes the two kinds that map cleanly to a chat history view:
//
//	{"version":"v1","kind":"Prompt","data":{"message_id":"...",
//	  "content":[{"kind":"text","data":"..."}],
//	  "meta":{"timestamp":1779081689}}}
//	{"version":"v1","kind":"AssistantMessage","data":{"message_id":"...",
//	  "content":[{"kind":"text","data":"..."}]}}
//
// Other kinds (tool_use, agent_message variants, etc.) are silently
// skipped so the schema can evolve without breaking pagination — a
// future sprint can extend the type-mapping without touching call sites.
//
// Why a callback for the session ID instead of a snapshot:
//
//	Like claudejsonl, the session ID can change mid-pagination (kiro
//	`session/load` followed by a fresh `session/new` swap, etc.). The
//	callback is re-invoked on every LoadBefore call so the next page
//	always reads from the latest jsonl path. Empty string ("") means
//	"no kiro session bound yet" — LoadBefore short-circuits to nil
//	rather than guessing a path.
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
	"github.com/naozhi/naozhi/internal/textutil"
)

// SessionIDFunc returns the kiro session ID for the bound session, or
// "" when no session has been negotiated yet. Re-evaluated on every
// LoadBefore call so a session/load transition is observed by the next
// page request.
type SessionIDFunc func() string

// maxFileBytes caps how many bytes LoadBefore reads from a single
// session jsonl. The default mirrors claudejsonl's per-session safety
// limit so a runaway transcript can't OOM the dashboard. Picked at
// package level so tests can shrink it without exporting state.
const maxFileBytes = 16 << 20 // 16 MiB

// ctxCheckEvery is how many parsed lines elapse between context.Done
// checks during LoadBefore. Trades a tiny constant overhead for prompt
// cancellation on large jsonl files. 100 mirrors the kiro chunk-rate
// observation in V5 (≈15 chunks/sec → ~7s of transcript per check).
const ctxCheckEvery = 100

// scanBufPool recycles the 64 KiB initial line buffer that bufio.Scanner
// would otherwise heap-allocate on every LoadBefore call. The dashboard
// paginates a session by issuing one LoadBefore per page, so a fresh 64 KiB
// alloc per page is pure churn (mirrors discovery/scanner.go's
// scanUserPromptBufPool and naozhilog/source.go's bufReaderPool).
var scanBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64*1024)
		return &b
	},
}

// kindPromptMarker and kindAsstMarker are byte quick-filters: a jsonl line
// that contains neither cannot decode into a Prompt or AssistantMessage
// record, so decodeLine can skip it before paying for two json.Unmarshal
// calls. Hoisted to package scope so the []byte literals do not allocate on
// every line of the hot scan loop (mirrors discovery/scanner.go's
// userTypeMarker).
var (
	kindPromptMarker = []byte(`"kind":"Prompt"`)
	kindAsstMarker   = []byte(`"kind":"AssistantMessage"`)
)

// Source is the kiro JSONL-backed history.Source.
type Source struct {
	rootDir   string        // ~/.kiro/sessions/cli — empty disables the source
	sessionID SessionIDFunc // produces the current kiro session ID
}

// New constructs a Source. If rootDir is empty or sessionIDFn is nil,
// the Source degrades to a zero-result implementation (equivalent to
// history.Noop) so misconfiguration never produces a nil-pointer panic
// at call time. Callers always get a non-nil *Source and can rely on
// LoadBefore to return (nil, nil) in degraded states.
func New(rootDir string, sessionIDFn SessionIDFunc) *Source {
	return &Source{rootDir: rootDir, sessionID: sessionIDFn}
}

// init registers this backend's factory with cli.Wrapper so any
// *cli.Wrapper constructed with BackendID="kiro" picks up the kiro
// jsonl history source automatically. Importing this package anywhere
// (cmd-level wireup or session.NewRouter side-effect) triggers the
// registration via Go's init order.
func init() {
	cli.RegisterHistoryFactory("kiro", factory)
}

// factory is the cli.HistoryFactoryFn for kiro. Returns
// cli.NoopHistorySource when the wiring lacks a KiroSessionsDir so
// misconfig at the router level still yields a non-nil source.
func factory(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
	if deps.KiroSessionsDir == "" {
		return cli.NoopHistorySource{}
	}
	return New(deps.KiroSessionsDir, s.SessionID)
}

// kiroRecord is the on-disk wrapper. data is held as RawMessage so the
// Prompt/AssistantMessage payloads can be decoded into kind-specific
// shapes without committing to a single schema for every record kind.
type kiroRecord struct {
	Version string          `json:"version"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`
}

// kiroContentChunk is one element inside a Prompt or AssistantMessage's
// content array. Only kind=="text" is rendered in the dashboard transcript;
// thinking / toolUse / toolResult / image chunks are silently dropped to
// match the Claude Code chat view contract (cc's discovery/history_tail.go
// assistant arm only surfaces b.Type == "text").
//
// Data is held as RawMessage rather than string because non-text chunks
// carry object payloads ("thinking" → {text, signature, redactedContent},
// "toolUse" → {toolUseId, name, input}). A `string` field would fail
// json.Unmarshal on the *whole* content array and silently drop every
// AssistantMessage that contained a tool_use or thinking block — which is
// nearly every record in a real kiro session.
type kiroContentChunk struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// kiroMessageData is the shared shape of Prompt.data and
// AssistantMessage.data. message_id is recorded but unused — the
// dashboard's UUID dedup uses the synthesised stamp from MergedSource.
type kiroMessageData struct {
	MessageID string             `json:"message_id"`
	Content   []kiroContentChunk `json:"content"`
	Meta      *kiroMessageMeta   `json:"meta,omitempty"`
}

// kiroMessageMeta carries the per-message timestamp. Only Prompt
// records observed with meta in V2; AssistantMessage records may omit
// meta and their entries are skipped because we cannot fabricate a
// time the way naozhilog can.
type kiroMessageMeta struct {
	Timestamp int64 `json:"timestamp"` // unix seconds
}

// LoadBefore returns up to `limit` entries strictly older than beforeMS
// from the kiro session's jsonl file, in chronological order
// (oldest → newest). When beforeMS <= 0 the upper bound is dropped and
// callers receive the newest `limit` entries.
//
// Errors are returned to the caller as informational signals: the
// underlying contract treats them as end-of-history (history.Source
// godoc), so an unreadable jsonl falls through to MergedSource's
// non-fatal logging path rather than aborting pagination.
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
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

	// Defence-in-depth: SessionIDFunc is exported, so a future caller
	// (or a test) might supply a path-traversal sid like "../etc/passwd"
	// that filepath.Join would happily resolve outside rootDir. Today the
	// only producer is ManagedSession.SessionID, but the public API
	// contract has no validation — reject any sid containing a path
	// separator or "..". Treat the bad sid as "no session" rather than an
	// error so callers paginate to the noop tail without surfacing the
	// internal validation failure to dashboard users.
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

	// Read sequentially with a per-file byte cap. Kiro appends in
	// chronological order so we collect all entries that satisfy the
	// upper bound, then trim to the newest `limit` after sort. A reverse
	// reader would be marginally cheaper for huge files but adds risk
	// against partial-write tails (the writer is still appending while
	// we read); a forward stream that silently drops the last malformed
	// record is the simpler robust approach.
	entries := s.parseFile(ctx, f, beforeMS)

	// parseFile already returns chronological order; sort defensively in
	// case kiro ever interleaves out-of-order timestamps (currently it
	// does not).
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Time < entries[j].Time
	})

	if len(entries) > limit {
		// Keep the newest `limit` entries — pagination is a tail read.
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// parseFile streams the jsonl file, decoding each line into an
// EventEntry that satisfies the beforeMS upper bound. Unknown kinds,
// blank lines, malformed JSON, and missing timestamps are all
// individually skipped — a bad single line never poisons the rest of
// the file. Returns entries in arrival order (chronological per kiro's
// append contract).
//
// Real-world kiro AssistantMessage records do not carry meta.timestamp
// at all (only the originating Prompt does). To surface them in the
// dashboard, parseFile remembers the most recent Prompt ts and grants
// each subsequent AssistantMessage that ts plus a monotonic 1 ms
// offset. The offset stays well under 1000 (kiro Prompt timestamps are
// unix seconds, so adjacent prompts are ≥1000 ms apart) so chronology
// across prompts is preserved.
func (s *Source) parseFile(ctx context.Context, f *os.File, beforeMS int64) []cli.EventEntry {
	// Read the LAST maxFileBytes, not the first. kiro appends chronologically
	// to a single rotation-free file, so a long session can exceed the cap;
	// reading from offset 0 would surface only the oldest prompts and the
	// newest messages would never be parsed. Seek to the tail window and drop
	// the first (likely partial) line so the cap covers recent bytes.
	skipPartialFirstLine := false
	if fi, err := f.Stat(); err == nil && fi.Size() > maxFileBytes {
		if _, err := f.Seek(fi.Size()-maxFileBytes, io.SeekStart); err == nil {
			skipPartialFirstLine = true
		}
	}
	limited := io.LimitReader(f, maxFileBytes)
	scanner := bufio.NewScanner(limited)
	// Allow 1 MiB lines — assistant messages can be long. Default 64 KiB
	// would silently truncate on token-rich replies. The initial buffer is
	// pooled: bufio.Scanner only grows (never shrinks below) the slice we hand
	// it, so returning it at zero length recycles the 64 KiB backing array.
	bufPtr := scanBufPool.Get().(*[]byte)
	defer func() {
		b := (*bufPtr)[:0]
		*bufPtr = b
		scanBufPool.Put(bufPtr)
	}()
	scanner.Buffer(*bufPtr, 1<<20)
	if skipPartialFirstLine && scanner.Scan() {
		// Discard the partial line straddling the seek boundary.
	}

	out := make([]cli.EventEntry, 0, 16)
	processed := 0
	// State across lines for assistant-timestamp salvage.
	var lastPromptMS int64
	var asstOffset int64
	for scanner.Scan() {
		// Cooperative cancellation. Done lookups every ctxCheckEvery
		// lines keep the cost negligible while still guaranteeing
		// prompt return on shutdown / dashboard navigation.
		if processed%ctxCheckEvery == 0 {
			select {
			case <-ctx.Done():
				return out
			default:
			}
		}
		processed++

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Byte quick-filter (#2246): only Prompt / AssistantMessage records
		// decode into an entry. Skipping everything else here avoids the two
		// json.Unmarshal calls decodeLine would otherwise spend on tool_use,
		// system, and other kinds that are unconditionally dropped anyway.
		if !bytes.Contains(line, kindPromptMarker) && !bytes.Contains(line, kindAsstMarker) {
			continue
		}

		entry, ok := decodeLine(line, lastPromptMS, asstOffset)
		if !ok {
			continue
		}
		// Maintain the borrow state. A successful Prompt resets the
		// per-prompt offset; a successful AssistantMessage ticks the
		// offset so the next assistant in the same prompt window stays
		// strictly later. Assistants that fall back to their own
		// meta.timestamp (rare today, but the schema permits it) are
		// detected by ts != lastPromptMS+1+asstOffset and don't
		// advance the offset.
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
			continue
		}
		out = append(out, entry)
	}
	if err := scanner.Err(); err != nil {
		// Partial-write tails surface here as bufio errors. Treat as
		// end-of-file so the merge layer doesn't lose the entries we
		// already accumulated.
		slog.Debug("kirojsonl: scanner error treated as EOF", "err", err)
	}
	return out
}

// decodeLine parses one jsonl record into an EventEntry. Returns
// (EventEntry{}, false) when the line is unusable (malformed JSON,
// unknown kind, missing timestamp, or empty content) so the caller can
// skip without aborting the whole file.
//
// lastPromptMS / asstOffset thread the parseFile-level borrow state
// through so an AssistantMessage with no meta.timestamp inherits its
// owning Prompt's ts plus a monotonic offset. Pass (0, 0) for the
// stateless legacy semantics — orphan assistants are then dropped.
func decodeLine(line []byte, lastPromptMS, asstOffset int64) (cli.EventEntry, bool) {
	var rec kiroRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		// Silent skip: this is the partial-final-line case during
		// concurrent writes, plus any future schema additions that
		// emit non-JSON-friendly lines. Logging at debug rather than
		// warn so a single mid-write tail doesn't spam ops dashboards.
		slog.Debug("kirojsonl: skip malformed line", "err", err)
		return cli.EventEntry{}, false
	}

	var entryType string
	switch rec.Kind {
	case "Prompt":
		entryType = "user"
	case "AssistantMessage":
		// "text" matches the cc dashboard contract:
		// internal/discovery/history_tail.go emits Type:"text" for
		// assistant messages, and dashboard.js' eventHtml branch on
		// e.type === 'text' is what renders the markdown bubble.
		// Emitting "assistant" here would render through the unknown-
		// type fallback and produce a malformed card.
		entryType = "text"
	default:
		// Unknown / future kinds (tool_use, system, etc.) are skipped
		// rather than emitted as a generic "system" entry. Emitting
		// would risk surfacing internal kiro events in the chat view;
		// a follow-up sprint can map specific kinds explicitly.
		return cli.EventEntry{}, false
	}

	var data kiroMessageData
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		slog.Debug("kirojsonl: skip line with bad data payload", "kind", rec.Kind, "err", err)
		return cli.EventEntry{}, false
	}

	timeMS, ok := extractTimestampMS(data.Meta)
	if !ok {
		// AssistantMessage in real kiro sessions never carries
		// meta.timestamp, so borrow the most recent Prompt ts plus a
		// monotonic offset to anchor the assistant on the timeline.
		// Prompts that miss their own ts (and any assistant before the
		// first ts-bearing prompt) can't be anchored — drop them
		// rather than forge ts=0, which would corrupt the strict-<
		// pagination boundary by collapsing many records to epoch.
		if entryType != "text" || lastPromptMS <= 0 {
			return cli.EventEntry{}, false
		}
		timeMS = lastPromptMS + 1 + asstOffset
	}

	fullText := concatTextChunks(data.Content)

	// Strict cc-parity for AssistantMessage: drop the entry entirely
	// when the model produced no plain-text output (only thinking +
	// tool_use + an empty placeholder text chunk). Without this, every
	// tool-driven turn injects a blank card into the transcript.
	// Prompts (user messages) keep the legacy permissive behaviour:
	// surface even an empty Summary so pagination time cursors advance.
	if entryType == "text" && strings.TrimSpace(fullText) == "" {
		return cli.EventEntry{}, false
	}

	// Truncate to the same caps the claude path uses (history_tail.go): a
	// 120-rune Summary and a 16000-rune Detail. Without this the full message
	// (up to the 1 MiB/line scanner limit) flows verbatim across the WS
	// boundary, and the dashboard renders an unbounded mega-bubble.
	summary, detail := textutil.TruncateRunesPair(fullText, 120, 16000)

	// Derive a deterministic UUID so MergedSource can dedup overlapping
	// pages. Without it the entry carries an empty UUID, which
	// merged.Source bypasses entirely (see internal/history/merged/source.go)
	// — so on LoadBefore overlap windows the same kiro line would surface
	// twice at the merge boundary. The Claude JSONL reader does the same via
	// textutil.DeriveLegacyUUID (internal/discovery/history_tail.go). kiro
	// EventEntries only ever populate Time/Type/Summary, so detail is "" to
	// match the fields actually present.
	return cli.EventEntry{
		UUID:    textutil.DeriveLegacyUUID(timeMS, entryType, summary, ""),
		Time:    timeMS,
		Type:    entryType,
		Summary: summary,
		Detail:  detail,
	}, true
}

// extractTimestampMS converts a kiro Prompt/AssistantMessage timestamp
// (unix seconds, integer) to unix milliseconds. Returns (0, false)
// when the meta block is missing or the timestamp is non-positive —
// those entries are dropped by decodeLine.
func extractTimestampMS(meta *kiroMessageMeta) (int64, bool) {
	if meta == nil || meta.Timestamp <= 0 {
		return 0, false
	}
	return meta.Timestamp * 1000, true
}

// concatTextChunks joins all kind=="text" chunks into a single string
// with no separator. Kiro typically emits one text chunk per message but
// the schema is a list, so handle multi-chunk defensively. Non-text
// chunks (thinking, toolUse, toolResult, image, ...) are skipped — they
// have no plain-text representation in the dashboard chat view, and the
// cc dashboard makes the same trade-off in
// discovery/history_tail.go's assistant arm.
//
// Each chunk's Data is a json.RawMessage. For text chunks we expect a
// JSON string; a malformed chunk is skipped silently (matches the rest
// of decodeLine's tolerance for partial-write tails).
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
			// Future schema drift (e.g. text payload becomes an object)
			// should not break the rest of the message. Drop just this
			// chunk; thinking/toolUse already pass through this same
			// silent-skip path via the kind filter above.
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
