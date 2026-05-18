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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/naozhi/naozhi/internal/cli"
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
// content array. Only kind=="text" is consumed today.
type kiroContentChunk struct {
	Kind string `json:"kind"`
	Data string `json:"data"`
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

	path := filepath.Join(s.rootDir, sid+".jsonl")
	f, err := os.Open(path) // #nosec G304 -- sid is sourced from the live session view, not user input
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
func (s *Source) parseFile(ctx context.Context, f *os.File, beforeMS int64) []cli.EventEntry {
	limited := io.LimitReader(f, maxFileBytes)
	scanner := bufio.NewScanner(limited)
	// Allow 1 MiB lines — assistant messages can be long. Default 64 KiB
	// would silently truncate on token-rich replies.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	out := make([]cli.EventEntry, 0, 16)
	processed := 0
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

		entry, ok := decodeLine(line)
		if !ok {
			continue
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
func decodeLine(line []byte) (cli.EventEntry, bool) {
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
		entryType = "assistant"
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
		// Cannot place the entry on the dashboard timeline without a
		// real timestamp — skip rather than synthesise. Forging a
		// time would corrupt the "load earlier" upper-bound contract.
		return cli.EventEntry{}, false
	}

	summary := concatTextChunks(data.Content)
	if summary == "" {
		// Empty content rows do appear in v1 (e.g. user-cancelled
		// prompts). Surface them as zero-length entries rather than
		// dropping so pagination time-cursors keep moving.
		summary = ""
	}

	return cli.EventEntry{
		Time:    timeMS,
		Type:    entryType,
		Summary: summary,
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

// concatTextChunks joins all text-kind chunks into a single string with
// no separator. Kiro typically emits one chunk per message but the
// schema is a list, so handle multi-chunk defensively. Non-text chunks
// (image, tool_call_request, ...) are skipped — they have no plain
// text representation in the dashboard chat view.
func concatTextChunks(chunks []kiroContentChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	if len(chunks) == 1 && chunks[0].Kind == "text" {
		return chunks[0].Data
	}
	total := 0
	for _, c := range chunks {
		if c.Kind == "text" {
			total += len(c.Data)
		}
	}
	if total == 0 {
		return ""
	}
	buf := make([]byte, 0, total)
	for _, c := range chunks {
		if c.Kind == "text" {
			buf = append(buf, c.Data...)
		}
	}
	return string(buf)
}
