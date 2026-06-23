// Package codexjsonl implements history.Source on top of the codex CLI's
// per-session rollout transcripts under ~/.codex/sessions.
//
// Unlike claude (project-slug dirs) or kiro (flat <sid>.jsonl), codex
// persists each thread as a date-bucketed rollout file whose name embeds
// the thread UUID:
//
//	~/.codex/sessions/YYYY/MM/DD/rollout-<ISO8601>-<threadId>.jsonl
//
// The threadId is the same UUID naozhi captures from thread/start, so the
// source globs for the suffix `-<threadId>.jsonl` across the date tree
// rather than composing a single deterministic path.
//
// Each line is a self-describing record with a top-level "type" + ISO-8601
// "timestamp". We consume the two `event_msg` payloads that map cleanly to
// a chat-history view (codex's transcript is friendlier than kiro's — these
// lines carry both a real timestamp AND already-joined plain text):
//
//	{"timestamp":"2026-06-21T11:53:07.956Z","type":"event_msg",
//	  "payload":{"type":"user_message","message":"..."}}
//	{"timestamp":"2026-06-21T11:53:11.127Z","type":"event_msg",
//	  "payload":{"type":"agent_message","message":"..."}}
//
// Other line types (session_meta, turn_context, response_item,
// token_count, task_started/complete) are silently skipped so the schema
// can evolve without breaking pagination.
package codexjsonl

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
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/textutil"
)

// SessionIDFunc returns the codex thread ID for the bound session, or ""
// when no thread has been negotiated yet. Re-evaluated on every LoadBefore
// call so a thread/resume transition is observed by the next page request.
type SessionIDFunc func() string

// maxFileBytes caps how many bytes LoadBefore reads from a single rollout
// file. Mirrors kirojsonl's per-session safety limit so a runaway
// transcript can't OOM the dashboard.
const maxFileBytes = 16 << 20 // 16 MiB

// ctxCheckEvery is how many parsed lines elapse between context.Done
// checks during parsing. Mirrors kirojsonl.
const ctxCheckEvery = 100

// scanBufPool recycles the 64 KiB initial line buffer that bufio.Scanner
// would otherwise heap-allocate on every parseFile call. The dashboard
// paginates a session by issuing one LoadBefore per page, so a fresh 64 KiB
// alloc per page is pure churn (mirrors kirojsonl's scanBufPool).
var scanBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64*1024)
		return &b
	},
}

// Source is the codex rollout-JSONL-backed history.Source.
type Source struct {
	rootDir   string        // ~/.codex/sessions — empty disables the source
	sessionID SessionIDFunc // produces the current codex thread ID
}

// New constructs a Source. If rootDir is empty or sessionIDFn is nil, the
// Source degrades to a zero-result implementation so misconfiguration never
// produces a nil-pointer panic. Callers always get a non-nil *Source and
// can rely on LoadBefore returning (nil, nil) in degraded states.
func New(rootDir string, sessionIDFn SessionIDFunc) *Source {
	return &Source{rootDir: rootDir, sessionID: sessionIDFn}
}

// init registers this backend's factory so any *cli.Wrapper constructed
// with BackendID="codex" picks up the codex rollout history source
// automatically when this package is blank-imported (wireup).
func init() {
	cli.RegisterHistoryFactory("codex", factory)
}

// factory is the cli.HistoryFactoryFn for codex. Returns
// cli.NoopHistorySource when the wiring lacks a CodexSessionsDir so a
// router-level misconfig still yields a non-nil source.
func factory(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
	if deps.CodexSessionsDir == "" {
		return cli.NoopHistorySource{}
	}
	return New(deps.CodexSessionsDir, s.SessionID)
}

// codexRecord is the on-disk line wrapper. Payload is held as RawMessage so
// only the event_msg lines we care about pay the second decode.
type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexEventMsg is the payload of an event_msg line. message is the
// already-joined plain text for user_message / agent_message.
type codexEventMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// LoadBefore returns up to `limit` entries strictly older than beforeMS for
// the bound codex thread, in chronological order (oldest → newest). When
// beforeMS <= 0 the upper bound is dropped and callers receive the newest
// `limit` entries.
//
// Errors are informational: the history.Source contract treats them as
// end-of-history, so an unreadable rollout falls through to MergedSource's
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

	// Defence-in-depth: SessionIDFunc is exported, so reject a sid that
	// could escape rootDir via the glob pattern. Treat a bad sid as "no
	// session" rather than an error (matches kirojsonl).
	if strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		slog.Warn("codexjsonl: refusing sid containing path separator or '..'",
			"sid_len", len(sid))
		return nil, nil
	}

	path, err := s.findRollout(sid)
	if err != nil || path == "" {
		// No matching rollout yet (new thread, or codex hasn't flushed).
		return nil, nil
	}

	f, err := os.Open(path) // #nosec G304 -- path resolved from a glob rooted at rootDir; sid validated above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("codexjsonl: open %s: %w", path, err)
	}
	defer f.Close()

	entries := s.parseFile(ctx, f, beforeMS)

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Time < entries[j].Time
	})

	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// findRollout locates the rollout file whose name ends in
// `-<sid>.jsonl` under the date-bucketed tree. codex names files
// rollout-<ISO8601>-<threadId>.jsonl inside YYYY/MM/DD/, so a recursive
// match on the suffix is the robust lookup (the leading timestamp is not
// known to naozhi). When multiple match (should not happen — threadId is a
// UUID), the lexicographically last is returned so a resumed/forked thread
// reading the freshest file wins.
func (s *Source) findRollout(sid string) (string, error) {
	suffix := "-" + sid + ".jsonl"
	var best string
	walkErr := filepath.WalkDir(s.rootDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// Unreadable subdir: skip it, keep walking the rest.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, suffix) {
			if p > best {
				best = p
			}
		}
		return nil
	})
	if walkErr != nil && best == "" {
		return "", walkErr
	}
	return best, nil
}

// parseFile streams the rollout file, decoding each event_msg line that
// satisfies the beforeMS upper bound. Blank lines, malformed JSON, unknown
// types, and unparseable timestamps are individually skipped so a bad line
// never poisons the rest of the file. Returns entries in arrival order
// (chronological per codex's append contract).
func (s *Source) parseFile(ctx context.Context, f *os.File, beforeMS int64) []cli.EventEntry {
	// Read the LAST maxFileBytes of the file, not the first. codex appends
	// chronologically with no rotation, so a long agentic session can exceed
	// the cap; reading from offset 0 would surface only the oldest turns and
	// the newest messages would never be parsed. Seek to the tail window and
	// drop the first (likely partial) line so the cap covers recent bytes.
	skipPartialFirstLine := false
	if fi, err := f.Stat(); err == nil && fi.Size() > maxFileBytes {
		if _, err := f.Seek(fi.Size()-maxFileBytes, io.SeekStart); err == nil {
			skipPartialFirstLine = true
		}
	}
	limited := io.LimitReader(f, maxFileBytes)
	scanner := bufio.NewScanner(limited)
	// Allow 1 MiB lines — assistant messages can be long; the default 64 KiB
	// would truncate token-rich replies. The initial buffer is pooled:
	// bufio.Scanner only grows (never shrinks below) the slice we hand it, so
	// returning it at zero length recycles the 64 KiB backing array.
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
	for scanner.Scan() {
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
		slog.Debug("codexjsonl: scanner error treated as EOF", "err", err)
	}
	return out
}

// decodeLine parses one rollout record into an EventEntry. Returns
// (EventEntry{}, false) when the line is not a renderable event_msg
// (user_message / agent_message), is malformed, has no parseable timestamp,
// or carries empty text.
func decodeLine(line []byte) (cli.EventEntry, bool) {
	var rec codexRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		slog.Debug("codexjsonl: skip malformed line", "err", err)
		return cli.EventEntry{}, false
	}
	if rec.Type != "event_msg" || len(rec.Payload) == 0 {
		return cli.EventEntry{}, false
	}

	var ev codexEventMsg
	if err := json.Unmarshal(rec.Payload, &ev); err != nil {
		slog.Debug("codexjsonl: skip line with bad payload", "err", err)
		return cli.EventEntry{}, false
	}

	var entryType string
	switch ev.Type {
	case "user_message":
		entryType = "user"
	case "agent_message":
		// "text" matches the cc dashboard contract — dashboard.js renders the
		// markdown bubble on e.type === 'text'. Emitting "assistant" would
		// fall through to the unknown-type card.
		entryType = "text"
	default:
		// system / reasoning / token_count / task_* lines are not chat bubbles.
		return cli.EventEntry{}, false
	}

	if strings.TrimSpace(ev.Message) == "" {
		return cli.EventEntry{}, false
	}

	timeMS, ok := parseISOms(rec.Timestamp)
	if !ok {
		return cli.EventEntry{}, false
	}

	// Truncate to the same caps the claude path uses (history_tail.go): a
	// 120-rune Summary and a 16000-rune Detail. Without this the full message
	// (up to the 1 MiB/line scanner limit) flows verbatim across the WS
	// boundary, and the dashboard renders an unbounded mega-bubble.
	summary, detail := textutil.TruncateRunesPair(ev.Message, 120, 16000)
	return cli.EventEntry{
		Time: timeMS,
		Type: entryType,
		// Derive a deterministic UUID so merged.Source can dedup overlapping
		// pages. Without it the entry carries an empty UUID, which
		// merged.mergeSorted treats as un-dedupable — the same codex line then
		// renders twice whenever a LoadBefore `beforeMS` cursor straddles a
		// previously-returned entry. claude (via discovery.history_tail) and
		// kiro (kirojsonl) both set this; codex must match the contract.
		// Derivation uses an empty detail arg to match kiro's pinned key.
		UUID:    textutil.DeriveLegacyUUID(timeMS, entryType, summary, ""),
		Summary: summary,
		Detail:  detail,
	}, true
}

// parseISOms converts codex's ISO-8601 RFC3339 timestamp (e.g.
// "2026-06-21T11:53:07.956Z") to unix milliseconds. Returns (0, false) on
// an unparseable or non-positive value so the entry is dropped rather than
// collapsed to epoch (which would corrupt the strict-< pagination cursor).
func parseISOms(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	ms := t.UnixMilli()
	if ms <= 0 {
		return 0, false
	}
	return ms, true
}
