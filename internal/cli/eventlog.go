package cli

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const defaultEventLogSize = 500

// imageDataURIPrefix is the required leading substring for every entry in
// EventEntry.Images. Today the only producer is MakeThumbnail (process.go:853),
// which always returns "data:image/jpeg;base64,..." or "". Future refactors
// that allow other producers — for instance passing through a remote URL or an
// IM CDN link — MUST keep this prefix so the dashboard's <img src=...> render
// path cannot be coerced into fetching `javascript:`, `http://evil/`, or
// arbitrary `data:text/html` payloads. Legacy browsers historically did not
// block `javascript:` in <img src>, and defense-in-depth here is cheap.
// S15 (Round 174).
const imageDataURIPrefix = "data:image/"

// sanitizeImages drops any data URI that is not an image/* data URL. Empty
// strings are also stripped so a single skipped thumbnail does not leave a
// "" slot the dashboard would have to render defensively. Returns the input
// slice unchanged when every entry is already valid, avoiding an allocation
// on the happy path (MakeThumbnail conforming producer).
//
// paths is an optional index-aligned slice of workspace-relative paths
// (EventEntry.ImagePaths) that MUST be filtered in lock-step so the
// dashboard's click-thumbnail-for-original flow stays aligned with the
// thumbnail it drew. Pass nil when the caller has no paths. The returned
// filtered paths slice is nil when every Images entry was valid (no
// allocation) OR when every path was dropped.
func sanitizeImages(imgs []string) []string {
	out, _ := sanitizeImagesAligned(imgs, nil)
	return out
}

func sanitizeImagesAligned(imgs, paths []string) ([]string, []string) {
	if len(imgs) == 0 {
		return imgs, nil
	}
	allOK := true
	for _, s := range imgs {
		if s == "" || !strings.HasPrefix(s, imageDataURIPrefix) {
			allOK = false
			break
		}
	}
	if allOK {
		return imgs, paths
	}
	filtered := make([]string, 0, len(imgs))
	var filteredPaths []string
	if len(paths) > 0 {
		filteredPaths = make([]string, 0, len(imgs))
	}
	anyPath := false
	for i, s := range imgs {
		if s == "" || !strings.HasPrefix(s, imageDataURIPrefix) {
			continue
		}
		filtered = append(filtered, s)
		// Lock-step append to filteredPaths — NEVER skip an append when
		// `paths` is non-empty, otherwise `filtered[j]` stops matching
		// `filteredPaths[j]` and a dashboard thumbnail click could fetch
		// the bytes of a DIFFERENT image in the same message. The gate is
		// "filteredPaths was initialised", not "i < len(paths)": replayed
		// history (AppendBatch/InjectHistory) can feed untrusted
		// EventEntry values where len(ImagePaths) < len(Images); pad with
		// "" so the lightbox degrades to the thumbnail for that slot
		// instead of serving a sibling image.
		if filteredPaths != nil {
			var p string
			if i < len(paths) {
				p = paths[i]
			}
			filteredPaths = append(filteredPaths, p)
			if p != "" {
				anyPath = true
			}
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	if !anyPath {
		filteredPaths = nil
	}
	return filtered, filteredPaths
}

// EventEntry is a simplified event record for the dashboard.
type EventEntry struct {
	Time       int64    `json:"time"`                 // unix ms
	Type       string   `json:"type"`                 // init, thinking, tool_use, text, result, system, agent, todo, task_start, task_progress (also maps task_updated), task_done
	Summary    string   `json:"summary,omitempty"`    // brief description
	Cost       float64  `json:"cost,omitempty"`       // cumulative cost (result events only)
	Detail     string   `json:"detail,omitempty"`     // fuller content for terminal view
	Tool       string   `json:"tool,omitempty"`       // tool name for tool_use events
	Subagent   string   `json:"subagent,omitempty"`   // subagent_type or name (empty for team-only agents)
	TeamName   string   `json:"team_name,omitempty"`  // team grouping key for agent team members
	Background bool     `json:"background,omitempty"` // true for run_in_background team agents
	Images     []string `json:"images,omitempty"`     // thumbnail data URIs for user image uploads
	// ImagePaths is the workspace-relative path of the on-disk copy of each
	// inline image, index-aligned with Images. Populated opportunistically by
	// buildUserEntry when persistFileRefs persisted an image to the workspace
	// attachment directory. Consumed by the dashboard lightbox so clicking a
	// thumbnail can load the original via /api/sessions/attachment instead of
	// the downsampled data URI. An empty slot (e.g. persist failed, or a
	// legacy replayed event) falls back to the thumbnail. ALWAYS sanitized
	// before use: callers join it under the session workspace and must reject
	// any absolute or escaping path — validation lives in the HTTP handler,
	// not here, so persisted history is pass-through.
	ImagePaths []string `json:"image_paths,omitempty"`
	TaskID     string   `json:"task_id,omitempty"`     // agent task correlation ID
	ToolUseID  string   `json:"tool_use_id,omitempty"` // links Agent tool_use → task_started
	LastTool   string   `json:"last_tool,omitempty"`   // most recent tool in agent task
	ToolUses   int      `json:"tool_uses,omitempty"`   // tool call count in agent task
	Tokens     int      `json:"tokens,omitempty"`      // total tokens consumed by agent task
	DurationMS int      `json:"duration_ms,omitempty"` // elapsed ms for agent task
	Status     string   `json:"status,omitempty"`      // agent task status (completed, error, etc.)
}

// SubagentInfo holds display information about an active sub-agent in the current turn.
type SubagentInfo struct {
	Name       string `json:"name"`
	Activity   string `json:"activity,omitempty"`   // task description from agent event
	Background bool   `json:"background,omitempty"` // true for run_in_background agents
}

type subscriber struct {
	ch        chan struct{} // buffered(1)
	closeOnce sync.Once
}

// EventLog is a thread-safe, bounded event log backed by a ring buffer.
type EventLog struct {
	mu      sync.RWMutex
	entries []EventEntry // ring buffer, pre-allocated to maxSize
	head    int          // next write position
	count   int          // number of valid entries (0..maxSize)
	maxSize int

	// Cached summaries updated atomically on Append for efficient access
	// without copying all entries. atomic.Pointer[string] is type-safe vs
	// atomic.Value (which accepts any interface value); Load returns nil
	// when never stored, distinct from a stored empty string.
	lastPromptSummary   atomic.Pointer[string] // most recent "user" entry summary
	lastActivitySummary atomic.Pointer[string] // most recent "tool_use"/"thinking" entry summary

	// userTurnCount is a monotonic counter of "user" entries appended to this
	// log since the Process was spawned. Exposed on SessionSnapshot.MessageCount
	// for sidebar / main-header chip display. Counts every user prompt including
	// those replayed via AppendBatch from persistedHistory — Process.InjectHistory
	// after shim reconnect rebuilds the counter to match the historical turn
	// count (persistence layer re-runs AppendBatch on startup; there is no
	// spurious reset). Oldest entries evicted by the ring buffer do not
	// decrement the counter: the semantic is "cumulative turn count", not
	// "live entries".
	userTurnCount atomic.Int64

	// Per-turn sub-agent tracking: reset on "result"/"user" events.
	turnAgents []SubagentInfo // foreground agents in current turn; protected by mu
	bgAgents   []SubagentInfo // background (run_in_background) agents; cleared on turn boundaries like turnAgents; protected by mu

	// subMu is an RWMutex because the hot path notifySubscribers only reads
	// the subscribers map (iterate + non-blocking channel send, which is
	// goroutine-safe). Subscribe/Unsubscribe/CloseSubscribers mutate the map
	// and take the write lock. RLock lets many concurrent Appends proceed
	// against different sessions in parallel without serialising through a
	// single Mutex. R65-PERF-M-1.
	subMu       sync.RWMutex
	subscribers map[*subscriber]struct{}
	subsClosed  bool         // CloseSubscribers has been called; no new subscribers accepted
	subCount    atomic.Int32 // mirrors len(subscribers); lets notifySubscribers skip the lock when zero
}

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]EventEntry, maxSize)}
}

// applyEntryStateLocked updates per-turn agent tracking for a single entry.
// Caller MUST hold l.mu. Summary atomic writes are the caller's responsibility
// so that AppendBatch can coalesce multiple per-type updates into one Store.
func (l *EventLog) applyEntryStateLocked(e EventEntry) {
	switch e.Type {
	case "agent":
		label := e.Subagent
		if label == "" {
			label = e.TeamName
		}
		if label == "" {
			label = "agent"
		}
		info := SubagentInfo{Name: label, Activity: e.Summary, Background: e.Background}
		if e.Background {
			l.bgAgents = append(l.bgAgents, info)
		} else {
			l.turnAgents = append(l.turnAgents, info)
		}
	case "result", "user":
		l.turnAgents = l.turnAgents[:0]
		l.bgAgents = l.bgAgents[:0]
	}
}

// Append adds an entry to the log, overwriting the oldest entry when full.
// Signals all subscribers non-blockingly after appending.
func (l *EventLog) Append(e EventEntry) {
	l.mu.Lock()
	if e.Time == 0 {
		e.Time = time.Now().UnixMilli()
	}
	// Server-side enforcement that every image entry is a data:image/* URI.
	// Today's sole producer (MakeThumbnail) already conforms, but enforcing
	// the contract here rather than trusting callers means any future
	// producer that accidentally passes through an external URL or a
	// javascript: URI gets stripped before it can reach the dashboard's
	// <img src=...> render path. S15 (Round 174).
	e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
	l.entries[l.head] = e
	l.head = (l.head + 1) % l.maxSize
	if l.count < l.maxSize {
		l.count++
	}

	l.applyEntryStateLocked(e)

	// Atomic summary stores are issued *inside* l.mu so that AppendBatch,
	// which holds l.mu for its full duration, cannot have its later Store
	// racing with a concurrent live Append's Store — the serialization on
	// l.mu guarantees last-writer-wins matches entry-order, not
	// entry-ordering-inverted by lock release scheduling.
	switch e.Type {
	case "user":
		storeAtomicString(&l.lastPromptSummary, e.Summary)
		l.userTurnCount.Add(1)
	case "tool_use", "thinking", "agent", "task_start", "task_progress", "todo":
		storeAtomicString(&l.lastActivitySummary, e.Summary)
	}

	l.mu.Unlock()

	l.notifySubscribers()
}

// AppendBatch adds multiple entries to the log, holding the lock once and
// notifying subscribers once. Used by InjectHistory to avoid per-entry overhead.
//
// Mirrors Append's per-entry sub-agent tracking and summary atomics so the
// sidebar does not show stale "(no prompt)" placeholders after history
// injection until a live event arrives. Atomic summary writes happen under
// l.mu to avoid a race with concurrent Append: if a live event ran Store
// after our Unlock but before our own Store, our older batch value would
// clobber it.
func (l *EventLog) AppendBatch(entries []EventEntry) {
	if len(entries) == 0 {
		return
	}
	var (
		lastPrompt, lastActivity string
		sawPrompt, sawActivity   bool
		userDelta                int64
	)
	// Capture a single wall-clock read before locking so the N zero-time
	// entries inside the loop (typical case: InjectHistory's 500-entry
	// replay on shim reconnect) don't each fire a vDSO call under l.mu.
	// Correctness: entries with an explicit Time are unaffected; entries
	// without one are assigned a monotonically-close "now" that is as
	// semantically correct as the per-entry reads they replace, while
	// keeping the write-lock hold time bounded. R71-PERF-L2.
	defaultTime := time.Now().UnixMilli()
	l.mu.Lock()
	for _, e := range entries {
		if e.Time == 0 {
			e.Time = defaultTime
		}
		// S15 (Round 174): same enforcement as Append. Replays from history
		// (InjectHistory → AppendBatch) should never contain non-image data
		// URIs today, but defense-in-depth is trivially cheap and locks the
		// contract to a single sink.
		e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
		l.entries[l.head] = e
		l.head = (l.head + 1) % l.maxSize
		if l.count < l.maxSize {
			l.count++
		}

		l.applyEntryStateLocked(e)

		// Track last-of-kind summaries so a single Store (below, still
		// under l.mu) captures the tail of the batch. The "saw" flag is
		// separate from the value so an empty final Summary still
		// overwrites the atomic — Append stores unconditionally for these
		// types, and diverging here would leave stale summaries visible.
		switch e.Type {
		case "user":
			lastPrompt = e.Summary
			sawPrompt = true
			userDelta++
		case "tool_use", "thinking", "agent", "task_start", "task_progress", "todo":
			lastActivity = e.Summary
			sawActivity = true
		}
	}

	if sawPrompt {
		storeAtomicString(&l.lastPromptSummary, lastPrompt)
	}
	if sawActivity {
		storeAtomicString(&l.lastActivitySummary, lastActivity)
	}
	if userDelta > 0 {
		// Single atomic add mirrors the lastPromptSummary single Store above —
		// callers observe the batch's cumulative impact in one step. Under l.mu
		// so the count is seen by any concurrent Snapshot that also reads
		// other per-turn state.
		l.userTurnCount.Add(userDelta)
	}
	l.mu.Unlock()

	l.notifySubscribers()
}

// notifySubscribers wakes all subscriber channels non-blockingly.
//
// Holds subMu as a reader for the full iteration: CloseSubscribers takes the
// write lock and uses sub.closeOnce to ensure each channel is closed exactly
// once. The send-on-closed-chan race is avoided by the RWMutex rather than
// by the channel send itself — Go's channel-send-is-goroutine-safe guarantee
// does NOT extend to sending on a closed channel, which panics. Multiple
// concurrent notifySubscribers readers are safe to iterate and signal the
// same channel set because non-blocking sends on an open channel are allowed
// to race.
//
// Fast path: idle sessions (no dashboard clients) check an atomic counter
// and skip subMu entirely. Append is invoked per content block on every
// stream-json event, so shaving one lock per assistant turn matters when
// N sessions run unattended. R65-PERF-M-1 upgraded from Mutex to RWMutex so
// concurrent notify calls from different sessions no longer serialise.
func (l *EventLog) notifySubscribers() {
	if l.subCount.Load() == 0 {
		return
	}
	l.subMu.RLock()
	for sub := range l.subscribers {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
	l.subMu.RUnlock()
}

// Subscribe returns a notification channel and an unsubscribe function.
// The channel receives a signal (non-blocking) whenever Append is called.
//
// If CloseSubscribers has already been called (process is dying), returns a
// channel that is already closed so the caller's select-on-notify arm fires
// immediately instead of parking forever. Without this guard, a Subscribe
// racing with readLoop's deferred CloseSubscribers would lazily rebuild the
// subscribers map and register a channel that nothing will ever close, so
// the downstream eventPushLoop would hang on <-notify until Hub shutdown.
func (l *EventLog) Subscribe() (<-chan struct{}, func()) {
	sub := &subscriber{ch: make(chan struct{}, 1)}
	l.subMu.Lock()
	if l.subsClosed {
		l.subMu.Unlock()
		sub.closeOnce.Do(func() { close(sub.ch) })
		return sub.ch, func() {}
	}
	if l.subscribers == nil {
		l.subscribers = make(map[*subscriber]struct{})
	}
	l.subscribers[sub] = struct{}{}
	// Add/sub counter pattern rather than re-deriving from len(map) — avoids
	// the map-header read on each mutation and makes the reader/writer
	// asymmetry explicit (Load is on the hot notify path, Add is rare).
	// R65-PERF-L-4.
	l.subCount.Add(1)
	l.subMu.Unlock()

	unsub := func() {
		l.subMu.Lock()
		if _, ok := l.subscribers[sub]; ok {
			delete(l.subscribers, sub)
			l.subCount.Add(-1)
		}
		l.subMu.Unlock()
		sub.closeOnce.Do(func() { close(sub.ch) })
	}
	return sub.ch, unsub
}

// CloseSubscribers closes all subscriber channels and clears the subscriber list.
// Called when the process dies so that eventPushLoop goroutines can exit.
// After this returns, subsequent Subscribe calls receive a pre-closed channel.
func (l *EventLog) CloseSubscribers() {
	if l == nil {
		return
	}
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for sub := range l.subscribers {
		sub.closeOnce.Do(func() { close(sub.ch) })
	}
	l.subscribers = nil
	l.subCount.Store(0)
	l.subsClosed = true
}

// Entries returns a copy of all entries in chronological order.
func (l *EventLog) Entries() []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]EventEntry, l.count)
	start := (l.head - l.count + l.maxSize) % l.maxSize
	for i := 0; i < l.count; i++ {
		out[i] = l.entries[(start+i)%l.maxSize]
	}
	return out
}

// LastN returns the most recent n entries in chronological order.
// If n <= 0 or n >= count, all entries are returned.
func (l *EventLog) LastN(n int) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.count
	if n > 0 && n < count {
		count = n
	}
	out := make([]EventEntry, count)
	start := (l.head - count + l.maxSize) % l.maxSize
	for i := 0; i < count; i++ {
		out[i] = l.entries[(start+i)%l.maxSize]
	}
	return out
}

// EntriesSince returns entries after the given unix ms timestamp, in chronological order.
// Single-pass backward scan collects matches into a reverse buffer; the caller
// receives them in chronological order. Previous implementation did two passes
// (count, then copy forward), touching each matched ring slot twice. For the
// hot streaming path (k = 1-5 new events per notify) the constant savings are
// small but the code path is simpler and avoids the arithmetic error surface
// of two separate modular indexing expressions.
func (l *EventLog) EntriesSince(afterMS int64) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.count == 0 {
		return nil
	}
	// First pass: collect matches in reverse order. Most calls match 0-5
	// entries so we allocate lazily only when the first match is found.
	var rev []EventEntry
	for i := l.count - 1; i >= 0; i-- {
		idx := (l.head - l.count + i + l.maxSize) % l.maxSize
		if l.entries[idx].Time <= afterMS {
			break
		}
		if rev == nil {
			// Typical streaming match count is 1-5; cap at a small constant
			// so sessions with hundreds of buffered entries don't allocate
			// a giant backing array on every notify. `append` will grow
			// organically if the match count exceeds this hint.
			initialCap := l.count - i
			if initialCap > 16 {
				initialCap = 16
			}
			rev = make([]EventEntry, 0, initialCap)
		}
		rev = append(rev, l.entries[idx])
	}
	if len(rev) == 0 {
		return nil
	}
	// Reverse in place — chronological order for the caller.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// EntriesBefore returns up to `limit` entries whose Time < beforeMS, in
// chronological order. Drives the dashboard "load earlier" pagination:
// caller passes the timestamp of the oldest currently-rendered event and
// gets the preceding page.
//
// A beforeMS of 0 is treated as "no upper bound" (equivalent to LastN).
// A non-positive limit returns nil.
func (l *EventLog) EntriesBefore(beforeMS int64, limit int) []EventEntry {
	if limit <= 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.count == 0 {
		return nil
	}

	// Walk backward from newest, skip entries whose Time >= beforeMS, collect
	// up to `limit` matches into a reverse buffer. Single pass keeps the code
	// symmetric with EntriesSince.
	//
	// Fast path: once we've seen an entry with Time < beforeMS, all earlier
	// entries in the ring also satisfy Time < beforeMS (entries are stored
	// in insertion/chronological order and Time is monotonic-ish from Append).
	// Switch from "skip then match" to "collect greedily" mode to avoid
	// re-evaluating the Time >= beforeMS condition for the remaining tail.
	// Before this, EntriesBefore on a 500-entry ring with beforeMS pointing
	// to the oldest page ran 500 iterations comparing timestamps; now it
	// runs up to ~`skip`+`limit` iterations.
	var rev []EventEntry
	crossed := beforeMS <= 0 // when beforeMS==0 treat as "no upper bound"
	for i := l.count - 1; i >= 0 && len(rev) < limit; i-- {
		idx := (l.head - l.count + i + l.maxSize) % l.maxSize
		if !crossed {
			if l.entries[idx].Time >= beforeMS {
				continue
			}
			crossed = true
		}
		if rev == nil {
			initialCap := limit
			if remaining := i + 1; remaining < initialCap {
				initialCap = remaining
			}
			rev = make([]EventEntry, 0, initialCap)
		}
		rev = append(rev, l.entries[idx])
	}
	if len(rev) == 0 {
		return nil
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// LastPromptSummary returns the summary of the most recent "user" entry.
func (l *EventLog) LastPromptSummary() string {
	return loadAtomicString(&l.lastPromptSummary)
}

// LastEntryOfType scans backward through the ring buffer and returns the most
// recent entry with the given type. Returns a zero EventEntry if none found.
func (l *EventLog) LastEntryOfType(typ string) EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := l.count - 1; i >= 0; i-- {
		idx := (l.head - l.count + i + l.maxSize) % l.maxSize
		if l.entries[idx].Type == typ {
			return l.entries[idx]
		}
	}
	return EventEntry{}
}

// LastActivitySummary returns the summary of the most recent "tool_use" or "thinking" entry.
func (l *EventLog) LastActivitySummary() string {
	return loadAtomicString(&l.lastActivitySummary)
}

// UserTurnCount returns the cumulative count of "user" entries appended to
// this log since the Process was spawned. Consumed by SessionSnapshot.MessageCount
// for sidebar / main-header display. Increments once per Append of a user entry
// and by the batch's user-entry count inside AppendBatch. Ring-buffer eviction
// does not decrement.
func (l *EventLog) UserTurnCount() int64 {
	return l.userTurnCount.Load()
}

// loadAtomicString returns the stored string or "" when the pointer is nil
// (never stored). Type-safe via atomic.Pointer[string]; no dynamic type check
// is needed.
func loadAtomicString(v *atomic.Pointer[string]) string {
	if p := v.Load(); p != nil {
		return *p
	}
	return ""
}

// storeAtomicString writes a string value through atomic.Pointer[string].
//
// Fast-path short-circuit (R176-PERF-P1): when the currently stored string
// equals s, skip the store entirely. Append runs storeAtomicString under
// l.mu for every user / tool_use / thinking / agent / task_start /
// task_progress / todo event, and the summaries are frequently repeated
// (e.g. a "Bash" tool_use fires the same one-liner on every step). By
// returning early on equality we avoid:
//
//  1. Allocating a fresh *string on the heap (the `&p` below forces
//     escape — on the slow path that's unavoidable because atomic.Pointer
//     must see a stable address; on the fast path we never take an
//     address at all, so escape analysis can keep s on the stack).
//  2. An atomic pointer write on a cache line that other goroutines' Load
//     paths (Snapshot, LastPromptSummary, LastActivitySummary) read at
//     high frequency.
//
// Safety: every caller writes while holding l.mu, so Load → compare →
// Store is atomic with respect to concurrent stores on this pointer.
// Concurrent readers either observe the old value (if we skipped) or the
// new value (if we wrote) — both valid prior-art snapshots.
func storeAtomicString(v *atomic.Pointer[string], s string) {
	if cur := v.Load(); cur != nil && *cur == s {
		return
	}
	p := new(string)
	*p = s
	v.Store(p)
}

// TurnAgents returns a copy of all currently active agents (foreground + background)
// in the current turn. Both are cleared on turn boundaries (result/user events).
// Returns nil when no agents are active.
func (l *EventLog) TurnAgents() []SubagentInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	total := len(l.turnAgents) + len(l.bgAgents)
	if total == 0 {
		return nil
	}
	out := make([]SubagentInfo, total)
	copy(out, l.turnAgents)
	copy(out[len(l.turnAgents):], l.bgAgents)
	return out
}

// TruncateRunes truncates s to at most maxRunes runes, appending "..." if truncated.
// Uses byte-level rune decoding to avoid allocating a full []rune slice.
func TruncateRunes(s string, maxRunes int) string {
	// Fast path for short strings: byte count is an upper bound on rune
	// count, so len(s) <= maxRunes guarantees no truncation is possible.
	// Tool names and short summaries ("Read", "Write") go through
	// TruncateRunes at ~5 events/s per active session; skipping the
	// utf8 decode loop eliminates a steady CPU baseline.
	if len(s) <= maxRunes {
		return s
	}
	i, count := 0, 0
	for i < len(s) {
		if count == maxRunes {
			return s[:i] + "..."
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s
}
