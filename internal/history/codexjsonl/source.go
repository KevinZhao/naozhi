// Package codexjsonl implements history.Source on top of the codex CLI's
// rollout transcripts, ~/.codex/sessions/YYYY/MM/DD/rollout-<ISO8601>-<threadId>.jsonl.
// threadId is the UUID naozhi captures from thread/start, so the source globs
// for the `-<threadId>.jsonl` suffix across the date tree. Only event_msg
// lines of type user_message / agent_message are consumed; other line types
// are skipped so the schema can evolve without breaking pagination.
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
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
)

// SessionIDFunc returns the codex thread ID for the bound session, or "" when
// none is negotiated yet. Re-evaluated on every LoadBefore call.
type SessionIDFunc func() string

// maxFileBytes caps how many bytes LoadBefore reads from one rollout file.
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

// maxLineBytes is the longest rollout record the scan accepts; longer records
// are skipped rather than aborting the scan (#2448).
const maxLineBytes = 1 << 20

// lineReaderPool recycles the bufio.Reader between file and scanner that lets
// an oversized record be drained and the scan resumed (see parseFile).
var lineReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

// Source is the codex rollout-JSONL-backed history.Source.
type Source struct {
	rootDir   string // ~/.codex/sessions — empty disables the source
	sessionID SessionIDFunc

	// findRollout caches the resolved rollout path per thread id (stable for the
	// session; the date tree can hold thousands of files). mu guards it.
	mu         sync.Mutex
	cachedSid  string
	cachedPath string
}

// New constructs a Source. Empty rootDir or nil sessionIDFn yields a
// zero-result Source (LoadBefore returns (nil, nil)) rather than a panic.
func New(rootDir string, sessionIDFn SessionIDFunc) *Source {
	return &Source{rootDir: rootDir, sessionID: sessionIDFn}
}

// init registers the codex history factory with cli.
func init() {
	cli.RegisterHistoryFactory("codex", factory)
}

// factory returns cli.NoopHistorySource when the wiring lacks a
// CodexSessionsDir so a router-level misconfig still yields a non-nil source.
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

// codexEventMsg is the payload of an event_msg line.
type codexEventMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// LoadBefore returns up to `limit` entries strictly older than beforeMS for
// the bound codex thread, oldest → newest; beforeMS <= 0 drops the bound.
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

	// SessionIDFunc is exported: reject a sid that could escape rootDir via
	// the glob pattern. A bad sid is "no session", not an error.
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

	// codex appends chronologically, so skip the O(n log n) sort unless the
	// slice is actually out of order.
	less := func(i, j int) bool { return entries[i].Time < entries[j].Time }
	if !sort.SliceIsSorted(entries, less) {
		sort.SliceStable(entries, less)
	}

	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// findRollout locates the rollout file ending in `-<sid>.jsonl` under the
// date tree (the leading timestamp is not known to naozhi). If several match,
// the lexicographically last wins so a resumed thread reads the freshest file.
func (s *Source) findRollout(sid string) (string, error) {
	// Cache hit. A stale path (file deleted/moved) is self-healing: os.Open
	// fails, LoadBefore returns (nil, nil), and the next distinct sid re-walks.
	s.mu.Lock()
	if s.cachedSid == sid && s.cachedPath != "" {
		p := s.cachedPath
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()

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
	// Only cache a non-empty hit; an empty result (no rollout flushed yet)
	// must keep re-walking until codex writes the file.
	if best != "" {
		s.mu.Lock()
		s.cachedSid = sid
		s.cachedPath = best
		s.mu.Unlock()
	}
	return best, nil
}

// parseFile streams the rollout file, decoding each event_msg line that
// satisfies the beforeMS bound. Blank, malformed, unknown-type and
// bad-timestamp lines are skipped individually. Returns arrival order.
func (s *Source) parseFile(ctx context.Context, f *os.File, beforeMS int64) []clievent.EventEntry {
	// Read the LAST maxFileBytes: codex appends with no rotation, so reading
	// from offset 0 would never reach the newest turns. Drop the partial first line.
	skipPartialFirstLine := false
	if fi, err := f.Stat(); err == nil && fi.Size() > maxFileBytes {
		if _, err := f.Seek(fi.Size()-maxFileBytes, io.SeekStart); err == nil {
			skipPartialFirstLine = true
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
	if skipPartialFirstLine && !scanner.Scan() && errors.Is(scanner.Err(), bufio.ErrTooLong) {
		// The partial first line is itself oversized. With an error set,
		// bufio.Scanner would hand the buffered prefix back as a final token
		// and it would decode as a whole record; drain and rebuild instead.
		if !discardRestOfLine(br) {
			return nil
		}
		scanner = bufio.NewScanner(br)
		scanner.Buffer(*bufPtr, maxLineBytes)
	}

	out := make([]clievent.EventEntry, 0, 16)
	processed := 0
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
		err := scanner.Err()
		if !errors.Is(err, bufio.ErrTooLong) {
			if err != nil {
				slog.Debug("codexjsonl: scanner error treated as EOF", "err", err)
			}
			return out
		}
		// One record exceeds maxLineBytes (#2448): drop the remainder and
		// resume with a fresh scanner so later records still surface.
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

// decodeLine parses one rollout record into an EventEntry; ok is false unless
// it is a non-empty user_message / agent_message with a parseable timestamp.
func decodeLine(line []byte) (clievent.EventEntry, bool) {
	var rec codexRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		slog.Debug("codexjsonl: skip malformed line", "err", err)
		return clievent.EventEntry{}, false
	}
	if rec.Type != "event_msg" || len(rec.Payload) == 0 {
		return clievent.EventEntry{}, false
	}

	var ev codexEventMsg
	if err := json.Unmarshal(rec.Payload, &ev); err != nil {
		slog.Debug("codexjsonl: skip line with bad payload", "err", err)
		return clievent.EventEntry{}, false
	}

	var entryType string
	switch ev.Type {
	case "user_message":
		entryType = "user"
	case "agent_message":
		// "text" is what dashboard.js renders as a markdown bubble;
		// "assistant" would fall through to the unknown-type card.
		entryType = "text"
	default:
		// system / reasoning / token_count / task_* lines are not chat bubbles.
		return clievent.EventEntry{}, false
	}

	if strings.TrimSpace(ev.Message) == "" {
		return clievent.EventEntry{}, false
	}

	timeMS, ok := parseISOms(rec.Timestamp)
	if !ok {
		return clievent.EventEntry{}, false
	}

	// Caps and the deterministic dedup UUID come from the shared recipe (#2336).
	return history.NewDerivedEntry(timeMS, entryType, ev.Message), true
}

// parseISOms converts codex's RFC3339 timestamp to unix milliseconds; (0, false)
// on an unparseable or non-positive value so the entry is dropped rather than
// collapsed to epoch (which would corrupt the strict-< cursor).
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
