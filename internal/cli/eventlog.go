package cli

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

const defaultEventLogSize = 500

// setAgentInternalIDMaxScan caps how many ring-buffer entries
// SetAgentInternalID walks backwards looking for the matching "agent" /
// "task_start" entries. The pair is almost always within the last few dozen
// entries of the same turn; capping the scan keeps the EventLog wlock from
// being held for the full O(maxSize) walk while concurrent Append calls
// queue behind it. R225-PERF-13.
//
// R229-PERF-7: an "RLock-scan, upgrade to wlock on hit" variant was
// considered to let parallel SetAgentInternalID calls share the read phase,
// but Go's sync.RWMutex has no atomic upgrade primitive. RUnlock→Lock
// reopens the window where Append can rotate the ring head between the
// scan's idx capture and the write, landing the linkage in the wrong slot.
// Cap=50 + early break (foundAgent && foundTaskStart) already collapses
// the wlock window to <1µs in practice, so the simpler always-wlock path
// is the correctness-preserving optimum.
const setAgentInternalIDMaxScan = 50

// entriesSinceInitialCap caps the lazily-allocated reverse-buffer initial
// capacity used by EntriesSince. The streaming-tail use case (dashboard
// poller, agent_tailer) calls EntriesSince per Append-notify so the typical
// match count is 1-5. A small cap keeps sessions with a fully-populated
// 500-slot ring from allocating a 500-entry backing array on every notify;
// `append` still grows organically when a slow consumer catches up on a
// burst. 16 covers ~99% of streaming notifies without spilling — empirical
// EventLog batch sizes during multi-tool turns rarely exceed 8 events
// between subscriber reads.
const entriesSinceInitialCap = 16

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

// sanitizeImagesAligned drops any data URI that is not an image/* data URL
// and strips empty strings so a single skipped thumbnail does not leave a
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
//
// Two-pass design (R229-PERF-8): the first pass is a pure read scan that
// short-circuits on the first invalid entry and returns the inputs untouched
// when every URI is well-formed — the common case under MakeThumbnail. The
// second pass only runs when the fast path failed and is the only place that
// allocates (`filtered` and `filteredPaths`). Folding the two passes into a
// single allocate-then-fill loop would force the happy path (every Append +
// AppendBatch with images) to pay one slice allocation per call even when
// nothing needs filtering, defeating the "happy path is zero-alloc" invariant
// that justifies the redundant scan. Do NOT collapse into one loop without
// re-running the bench at internal/cli/eventlog_images_align_test.go.
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
	// UUID is a 32-char lowercase hex identity for this event,
	// assigned at Append-time by EventLog.stampUUID. Stable across
	// process restarts because it rides along with the entry into
	// the on-disk event log (internal/eventlog/persist). MergedSource
	// uses UUID as the exact-match dedup key between the local
	// JSONL tier and Claude CLI JSONL fallback — see RFC §3.5.2.
	//
	// "" means "legacy entry (from a pre-UUID persisted record or
	// a Claude JSONL replay that hasn't been fingerprinted yet)".
	// MergedSource handles the empty case by deriving a stable UUID
	// from (Time + Summary) so two replays of the same Claude record
	// land on the same key.
	UUID       string   `json:"uuid,omitempty"`
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
	DurationMS int64    `json:"duration_ms,omitempty"` // elapsed ms for agent task
	Status     string   `json:"status,omitempty"`      // agent task status (completed, error, etc.)
	// Agent team internal-view linkage (RFC v4 agent-team-ui §3.2.2).
	// All four fields are persisted to sessions/*.jsonl on "agent" and
	// "task_start" entries so SubagentLinker.SeedFromHistory can rebuild
	// the task_id → on-disk-transcript mapping after shim reconnect or
	// CLI-dead respawn without re-scanning ~/.claude/projects/.
	// Async backfilled via EventLog.SetAgentInternalID once the linker
	// resolves, hence all omitempty.
	TaskType        string `json:"task_type,omitempty"`         // "in_process_teammate" | "local_bash" | ""
	InternalAgentID string `json:"internal_agent_id,omitempty"` // "agent-<hex17>" filename stem under <projectDir>/<sessionID>/subagents/
	JSONLPath       string `json:"jsonl_path,omitempty"`        // absolute path to agent transcript jsonl
	FirstPromptID   string `json:"first_prompt_id,omitempty"`   // jsonl first-line promptId; guards against same-name re-spawn

	// AskQuestion carries the interactive AskUserQuestion card payload. Only
	// set on Type=="ask_question" entries synthesised from an AskUserQuestion
	// tool_use block — kept as a separate field (rather than stuffing JSON
	// into Detail) so the dashboard renderer doesn't have to re-parse and
	// so Go callers (EventLog replay → WS broadcast) don't pay a JSON
	// unmarshal per question bubble.
	AskQuestion *AskQuestion `json:"ask_question,omitempty"`

	// ToolCall is the per-event payload for ACP tool_call / tool_call_update
	// rich progress rows. Multi-Backend RFC §8.3 D17. Same struct on initial
	// invocation and updates; dashboard threads them by ID. Stream-json
	// (Claude) leaves nil and uses Type=="tool_use" with Tool name + Detail
	// for input.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// SubagentInfo holds display information about an active sub-agent in the current turn.
// Fields below "Background" are added by RFC v4 agent-team-ui §3.2.2 to surface
// per-agent linkage (task_id/tool_use_id), lifecycle status, and aggregator
// metrics. All values are derived from EventEntry fields or server-side tailer
// state (§3.5.4 enrichSnapshot); none are persisted independently — the
// canonical source remains the ring-buffered EventEntry list.
type SubagentInfo struct {
	Name       string `json:"name"`
	Activity   string `json:"activity,omitempty"`   // task description from agent event
	Background bool   `json:"background,omitempty"` // true for run_in_background agents
	TaskID     string `json:"task_id,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	TaskType   string `json:"task_type,omitempty"`
	// InternalAgentID mirrors EventEntry.InternalAgentID once SubagentLinker
	// resolves the task_id → on-disk agent-<hex>.jsonl mapping. Empty before
	// async Resolve completes (~0.1-3s grace) and on tombstoned tasks.
	InternalAgentID string `json:"internal_agent_id,omitempty"`
	Status          string `json:"status,omitempty"`        // "spawned" | "running" | "completed" | "error"
	StartedAtMS     int64  `json:"started_at_ms,omitempty"` // task_start wall-clock
	// Aggregator-injected fields (server.enrichSnapshot). LastTool/LastDetail
	// come from the silent tailer's parse of the agent transcript; ToolUses
	// and DurationMS use task_notification's usage payload when present,
	// otherwise the tailer's running counters.
	LastTool   string `json:"last_tool,omitempty"`
	LastDetail string `json:"last_detail,omitempty"`
	ToolUses   int    `json:"tool_uses,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// subscriber is the per-Subscribe handle. R229-PERF-12 reviewer
// suggested pooling these via sync.Pool to dodge alloc on dashboard
// reconnect spurts; that won't work because (a) closed channels cannot
// be re-used (the unsub path calls close(sub.ch), and reusing a closed
// channel makes a recv permanently return the zero value before any
// future notify lands), and (b) sync.Once cannot be reset to its
// pristine state without unsafe pointer surgery. The cheap path is
// `make(chan struct{}, 1) + sync.Once{}` per Subscribe — both are tiny
// allocations and tab-reload is human-cadence, so pooling buys ~2
// alloc/subscribe at most. Documented to discourage future
// "obvious" pool attempts that would silently break the close-once
// invariant.
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
	// lastResponseSummary tracks the most recent assistant "text" entry summary
	// so the sidebar can render a 30-rune dim second line under the prompt.
	// R110-P1: assistant response preview. Mirrors lastPromptSummary's atomic
	// store-on-Append discipline (under l.mu so AppendBatch's last-writer
	// ordering stays consistent). Lock-free read via LastResponseSummary().
	lastResponseSummary atomic.Pointer[string] // most recent assistant "text" entry summary

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

	// lastEventAt is the wall-clock (unix nano) of the most recent live
	// Append. Used by Router.Cleanup's stuckKill / idle_timeout checks as a
	// second-chance activity signal: the session-level lastActive is only
	// refreshed on Send entry, so a long-running turn (>2×TotalTimeout)
	// whose CLI is still streaming tool_use / thinking events would
	// otherwise be misclassified as "stuck" and killed. Any live event
	// (assistant, tool_use, thinking, agent, result, …) is enough to prove
	// the process is making progress. AppendBatch from InjectHistory /
	// recovery replays does NOT update this value — replayed entries have
	// historical timestamps and are not evidence of live activity.
	lastEventAt atomic.Int64

	// Per-turn sub-agent tracking: reset on "result"/"user" events.
	turnAgents []SubagentInfo // foreground agents in current turn; protected by mu
	bgAgents   []SubagentInfo // background (run_in_background) agents; cleared on turn boundaries like turnAgents; protected by mu

	// turnAgentCount mirrors len(turnAgents)+len(bgAgents) for lock-free
	// reads from the ManagedSession.Snapshot hot path. Most sessions sit at
	// zero outside an active subagent turn; the dashboard polls Snapshot at
	// 1Hz × N tabs × ~50 sessions, so taking l.mu RLock + allocating a
	// 0-length slice on every empty read is wasted work. Updated under
	// l.mu alongside the slice mutations in applyEntryStateLocked /
	// AppendBatch's bg/turn agent paths and the result/user reset. R220-PERF-6.
	turnAgentCount atomic.Int32

	// onAgentTaskDone fires after applyEntryStateLocked ingests a "task_done"
	// entry, carrying the task_id and final status. The server layer uses
	// this to close the corresponding agent tailer (RFC v4 §3.5.4). Fired
	// OUTSIDE l.mu to avoid back-pressure on hot Append paths; callers must
	// be fast + re-entrant safe (the agent_tailer.closeTask path is).
	// Zero/nil = no subscriber.
	//
	// R233B-PERF-6: stored via atomic.Pointer instead of mutex+func because
	// every Append touches the load path on the hot stream-event ingest
	// (50 sess × 50 evt/s ≈ 2500 reads/s). Mutex would serialise all those
	// reads through a single CAS even though writes (one SetOnAgentTaskDone
	// call per session lifetime) are vanishingly rare. Pointer mode matches
	// PersistSink / OnExecuteFunc / textutil.LoadAtomicString already used
	// across the codebase.
	onAgentTaskDoneFn atomic.Pointer[func(taskID, status string)]

	// subMu is an RWMutex because the hot path notifySubscribers only reads
	// the subscribers slice (iterate + non-blocking channel send, which is
	// goroutine-safe). Subscribe/Unsubscribe/CloseSubscribers mutate the slice
	// and take the write lock. RLock lets many concurrent Appends proceed
	// against different sessions in parallel without serialising through a
	// single Mutex. R65-PERF-M-1.
	//
	// R239-PERF-9: storage is `[]*subscriber` rather than `map[*subscriber]struct{}`.
	// notifySubscribers runs at ~25K calls/s in 500-session deployments —
	// mapiterinit + mapiternext on a 1-2 element map adds ~tens of ns per
	// call vs a slice loop. Unsubscribe stays O(N) via swap-to-end + truncate
	// under subMu.Lock; the closeOnce guard on subscriber preserves the
	// "channel closed exactly once" invariant regardless of unsubscribe
	// vs CloseSubscribers race ordering.
	subMu       sync.RWMutex
	subscribers []*subscriber
	subsClosed  bool         // CloseSubscribers has been called; no new subscribers accepted
	subCount    atomic.Int32 // mirrors len(subscribers); lets notifySubscribers skip the lock when zero

	// persistSink is the optional on-disk persistence hook. RFC
	// §3.2 / §3.3 cover the full contract; the two-atomic design
	// below serves as the runtime half of the "runtime + AST"
	// double-check on "SetPersistSink must run AFTER InjectHistory":
	//
	//   - sinkReady starts false. Every Append/AppendBatch that
	//     fires while sinkReady is false carries replayPhase=true
	//     to the sink (if one has already been stored), letting
	//     the Persister drop + counter rather than commit a replay
	//     loop to disk.
	//   - persistSinkPtr holds the sink closure. It is populated
	//     atomically by SetPersistSink, along with a Store(true)
	//     on sinkReady in the same method. Reads on the hot path
	//     Load() the pointer; nil means "no persistence configured",
	//     which is the zero-configuration default for tests and
	//     fake processes.
	//
	// The two-stage (sinkReady bool + sink pointer) construction
	// mirrors the schema-level invariant that schema.Record.ReplayPhase
	// is a declared field — but here ReplayPhase is derived at Append
	// time from sinkReady, not carried on EventEntry. Keeping the
	// EventEntry struct size constant matters because EventLog's
	// ring buffer pre-allocates maxSize entries.
	sinkReady      atomic.Bool
	persistSinkPtr atomic.Pointer[PersistSink]

	// persistSinkOnePtr is the optional single-entry fast-path sink,
	// installed via SetPersistSinkPair. When non-nil, Append's hot path
	// uses it instead of constructing a 1-slot []EventEntry{e} literal
	// before invokePersistSink — that literal heap-escapes through the
	// slice sink's retention contract (#410). AppendBatch never consults
	// this pointer; the multi-entry path always uses persistSinkPtr so
	// the persister observes contiguous batches.
	//
	// Lifetime: paired with persistSinkPtr by SetPersistSinkPair.
	// Callers using only the legacy SetPersistSink keep this nil and
	// pay the slice-literal allocation, preserving full backward
	// compatibility for sinks that have not opted in.
	persistSinkOnePtr atomic.Pointer[PersistSinkOne]

	// replayInvokeTotal counts how many invokePersistSink calls fired
	// while sinkReady was still false (replayPhase=true). This window
	// starts at construction and ends when SetPersistSink Stores
	// sinkReady=true; entries observed in this window are tagged for
	// the Persister to drop without committing to disk so an
	// InjectHistory replay cannot create a write-loop. R242-ARCH-20.
	//
	// The counter is purely diagnostic — production reads come from
	// /health (and tests via ReplayInvokeTotal()) to confirm the
	// SetPersistSink-after-InjectHistory ordering held in practice.
	// A non-zero steady-state value at runtime means a Append /
	// AppendBatch caller raced ahead of the persister attach, which
	// is a contract violation worth flagging in dashboards even when
	// the Persister silently absorbs the drop.
	replayInvokeTotal atomic.Int64
}

// PersistSinkOne is the single-entry counterpart to PersistSink. When
// installed alongside (or in lieu of) the slice-shaped PersistSink via
// SetPersistSinkPair, it is preferred by Append's hot path so the
// per-call `[]EventEntry{e}` literal allocation disappears (#410).
//
// Semantics match PersistSink exactly: the entry is the same value that
// would have landed at index 0 of the slice variant; replayPhase is
// derived from sinkReady identically. AppendBatch always uses the slice
// path — collapsing N>1 entries into N single-entry calls would lose the
// per-batch atomic write-order the persister relies on (see persister.go
// SinkFor batching contract).
//
// Implementations MUST be non-blocking, identical to PersistSink. Callers
// that retain the EventEntry past return must copy any reference fields
// they care about (Images / ImagePaths / AskQuestion / ToolCall) — the
// EventEntry struct itself is passed by value, but its slice/pointer
// fields share backing memory with the ring buffer slot.
type PersistSinkOne func(entry EventEntry, replayPhase bool)

// PersistSink is the event log's persistence hook contract.
// cli.EventLog calls the stored sink (when set) after every Append
// and AppendBatch, passing:
//
//   - entries: a defensive copy of the appended EventEntry values
//     (sink implementations may retain the slice — EventLog will
//     not modify it after the call returns).
//   - replayPhase: true while sinkReady is false (i.e. this Append
//     came before SetPersistSink was called). The Persister drops
//     and counter-metrics these batches so a broken call path
//     (InjectHistory after SetPersistSink) cannot create a
//     log-replay-amplification loop.
//
// Implementations MUST be non-blocking. Persister.SinkFor satisfies
// this contract by using a non-blocking channel send + drop policy;
// a custom sink may choose a different policy (synchronous disk
// commit in a test, metrics-only accounting) but must NEVER hold
// up the Append caller — EventLog takes pains to release l.mu
// before invoking the sink specifically so slow sinks can't stall
// the ring buffer.
//
// # Relationship with persist.PersistSink (R222-ARCH-4 anchor)
//
// internal/eventlog/persist also defines a `PersistSink` symbol —
// the on-disk Persister's accept-from-bridge hook. The two are
// deliberately distinct types today:
//
//   - cli.PersistSink (this type) takes []EventEntry, the cli-domain
//     wire shape. Lives next to EventLog because EventLog is the
//     producer.
//   - persist.PersistSink takes persist/schema entries, the on-disk
//     wire shape (uuid, replay flag, framing fields).
//
// session/eventlog_bridge.go is the only place that translates
// between them — capturing the cli-side slice, building the
// persist-side schema records, and forwarding to the Persister.
// R222-ARCH-4 / R227-ARCH-15 propose collapsing the two types onto
// a single internal/eventlog/schema struct so the bridge's marshal
// step disappears, but that requires moving EventEntry out of
// internal/cli (today multiple consumers in cli rely on the
// in-package type for sub-agent linkage). Until that lands, treat
// the two PersistSink names as a documented refactor seam, not a
// drift.
type PersistSink func(entries []EventEntry, replayPhase bool)

// SetPersistSink installs the on-disk persistence hook. See the
// PersistSink contract + the sinkReady field godoc for the full
// ordering rules.
//
// This method is the only public way to flip sinkReady to true.
// Calling it twice replaces the sink (last-writer-wins); calling
// it with nil "clears" the sink AND flips sinkReady back to false
// so that any subsequent SetPersistSink(real) re-enters the
// pre-attach phase cleanly. R20260526-GO-010: without this reset,
// a "pause persist → re-install sink → InjectHistory" sequence
// would tag the replay batch replayPhase=false (live) and the
// Persister would commit the duplicate history to disk.
//
// R224-GO-5 (closes TODO): the original review flagged a "race
// window between sink Store and sinkReady Store where one entry
// can be wrongly tagged replay=true". The current ordering
// (sink-first, sinkReady-second) is intentional and the asymmetry
// is on the safe side:
//
//   - Inverted order (sinkReady=true first) opens a window where
//     invokePersistSink loads a nil sink AFTER Append observed
//     sinkReady=true → the event is dropped on the floor with no
//     telemetry path to recover it.
//   - Current order (sink first, then sinkReady) opens a window
//     where Append observes sinkReady=false but the sink IS set;
//     the event reaches the Persister tagged replayPhase=true,
//     which is the SAME tag history-replay paths use. Persister
//     drops + counters those entries instead of committing them
//     to disk. The window is bounded by two atomic Stores
//     (sub-ns) and only fires for live events landing in that
//     gap on a freshly-attached sink — by definition the sink
//     just attached and there is no data to lose; the next event
//     after the second Store lands cleanly as live.
//
// Reversing the stores would be strictly worse: silent event loss
// vs the current "occasional belt-and-suspenders replay drop".
// Atomic.Pointer cannot carry both fields without a pointer
// allocation per Store, which would dominate the cost of a path
// that runs once per session lifetime. Keep the asymmetry.
func (l *EventLog) SetPersistSink(fn PersistSink) {
	if fn == nil {
		// Order: clear sinkReady FIRST so any concurrent Append racing
		// the uninstall observes the pre-attach phase before the sink
		// pointer goes nil. Storing the pointer first would open a
		// window where invokePersistSink loads a non-nil pointer but
		// reads sinkReady=true, then by the time it dispatches the
		// pointer is nil — same shape as the inverted-order race
		// documented in the install path. R20260526-GO-010.
		l.sinkReady.Store(false)
		l.persistSinkPtr.Store(nil)
		// Clear any previously paired single-entry sink — leaving it
		// installed would cause Append to fire the single-entry
		// closure while AppendBatch silently no-ops (slice ptr nil),
		// breaking the "consistent dispatch" invariant the two paths
		// share. SetPersistSinkPair is the only entrypoint that
		// installs a single sink; SetPersistSink-with-nil clears both
		// for symmetry.
		l.persistSinkOnePtr.Store(nil)
		return
	}
	// Store the sink pointer FIRST so any concurrent Append that
	// reads sinkReady=true will also see a valid sink. Without this
	// ordering there's a window where Append sees sinkReady=true
	// but Load returns nil, losing the event. See R224-GO-5 anchor
	// in the godoc above for the ordering proof.
	p := fn
	l.persistSinkPtr.Store(&p)
	// Installing a slice-only sink retracts any previously paired
	// single-entry sink: callers who switch back from the pair API to
	// the legacy slice API must not silently keep the old single sink
	// firing — the two slices may correspond to entirely different
	// downstream destinations.
	l.persistSinkOnePtr.Store(nil)
	l.sinkReady.Store(true)
}

// SetPersistSinkPair installs both the slice-shaped batch sink and a
// single-entry fast-path sink in one call. The two sinks MUST drain to
// the same downstream destination — Append uses `single`, AppendBatch
// uses `batch`, and the per-call decision is invisible to operators.
// When `single` is nil, behaviour collapses back to SetPersistSink(batch).
//
// Ordering matches SetPersistSink's documented R224-GO-5 contract: the
// sink pointers are stored before sinkReady flips to true so a concurrent
// Append observing sinkReady=true is guaranteed to see a non-nil sink
// for at least one dispatch path. The single-entry pointer is stored
// before the slice pointer so Append's "prefer single" dispatch never
// regresses to a slice-literal alloc once the pair has been installed.
//
// #410: the single-entry path lets Append skip the `[]EventEntry{e}`
// literal that would otherwise escape through the slice sink's retention
// contract, removing one heap alloc per live event on the hot path.
func (l *EventLog) SetPersistSinkPair(batch PersistSink, single PersistSinkOne) {
	if batch == nil {
		// Treat a nil batch as "uninstall everything" so callers do not
		// have to remember a separate clear sequence; mirrors
		// SetPersistSink(nil) semantics — including the sinkReady
		// reset that lets a subsequent re-install enter the pre-attach
		// phase cleanly. R20260526-GO-010.
		l.sinkReady.Store(false)
		l.persistSinkOnePtr.Store(nil)
		l.persistSinkPtr.Store(nil)
		return
	}
	bp := batch
	if single != nil {
		sp := single
		l.persistSinkOnePtr.Store(&sp)
	} else {
		l.persistSinkOnePtr.Store(nil)
	}
	l.persistSinkPtr.Store(&bp)
	l.sinkReady.Store(true)
}

// invokePersistSink is the Append / AppendBatch helper that fires
// the sink (when set) after the ring-buffer mutations are committed
// and l.mu has been released.
//
// replayPhase is derived from sinkReady at the time of the call —
// entries appended before SetPersistSink ran are replay-tagged,
// entries after are live.
//
// `entries` must be a slice that is safe for the sink to retain —
// callers pass a freshly-copied slice (not a view into the ring
// buffer) because the ring can wrap and overwrite slots shortly
// after.
func (l *EventLog) invokePersistSink(entries []EventEntry) {
	p := l.persistSinkPtr.Load()
	if p == nil {
		return
	}
	// When sinkReady is false the batch must be tagged replayPhase=true
	// — this is the runtime blocker-1 guard from RFC §3.2.3.
	replay := !l.sinkReady.Load()
	if replay {
		// R242-ARCH-20: count replay-phase invocations so /health (or
		// equivalent diagnostic endpoint) can surface a non-zero value
		// as a contract-violation signal. Steady-state production should
		// see this counter freeze at the InjectHistory replay total and
		// never grow once SetPersistSink has run.
		l.replayInvokeTotal.Add(1)
	}
	(*p)(entries, replay)
}

// invokePersistSinkOne is the single-entry counterpart to invokePersistSink,
// fired only by Append (not AppendBatch). Returns true when the single sink
// was attached and dispatched; false when the caller must fall back to the
// slice-shaped invokePersistSink path. Sharing the same replayPhase
// derivation + replayInvokeTotal counter as invokePersistSink keeps the
// telemetry surface unified: a sink-pair caller and a slice-only caller
// observe identical counter behaviour. (#410)
func (l *EventLog) invokePersistSinkOne(entry EventEntry) bool {
	p := l.persistSinkOnePtr.Load()
	if p == nil {
		return false
	}
	replay := !l.sinkReady.Load()
	if replay {
		l.replayInvokeTotal.Add(1)
	}
	(*p)(entry, replay)
	return true
}

// ReplayInvokeTotal returns the number of invokePersistSink calls that
// observed sinkReady=false (replayPhase=true). This is a diagnostic
// counter only: production code does not gate behaviour on it. Tests
// use it to assert that the SetPersistSink-after-InjectHistory ordering
// held; dashboards / /health endpoints can expose it to detect a
// pre-attach burst that would otherwise be silently absorbed by the
// Persister's replay-drop logic.
//
// R242-ARCH-20 (closed): the review asked for a `replayDropTotal
// atomic.Int64` exposed on /health to detect the SetPersistSink double-
// store ordering window misfiring in production. The counter pair is
// already in place across the cli ↔ persist boundary:
//
//   - cli side: ReplayInvokeTotal() above counts invokePersistSink calls
//     that fired with replayPhase=true (the cli's local view of "this
//     entry was tagged replay because sinkReady was false").
//   - persist side: persist.Stats().ReplayLeak (persister.replayLeakCnt)
//     counts entries the Persister received with replayPhase=true and
//     dropped on the floor, plus persist.Observer.OnReplayLeak fires per
//     batch for push-based monitoring.
//
// Operators wiring /health surface both values: cli's count > 0 with
// persist's count == 0 means the sink had not yet attached when the
// race fired (the harmless case the SetPersistSink godoc above
// documents); both > 0 means the InjectHistory replay batches were
// genuinely absorbed by the persister's replay-drop guard. The
// counters together fully cover the contract-violation surface the
// original review wanted observable; no additional `replayDropTotal`
// is needed because the boundary is two-sided and each side keeps its
// own honest count.
//
// Safe to call from any goroutine; returns the cumulative count from
// the EventLog's construction.
func (l *EventLog) ReplayInvokeTotal() int64 {
	return l.replayInvokeTotal.Load()
}

// SinkReady reports whether SetPersistSink has wired a persistence hook
// and toggled `sinkReady` to true. Designed for /health surfacing — pair
// with ReplayInvokeTotal() so operators can distinguish "the sink simply
// hasn't attached yet" (SinkReady=false, ReplayInvokeTotal frozen at the
// InjectHistory replay total) from "the SetPersistSink-after-Append
// ordering window opened in production" (SinkReady=true,
// ReplayInvokeTotal still climbing — should be statistically impossible
// under correct caller ordering).
//
// R242-ARCH-20 (closes the diagnostic surface the original review asked
// for). The counter pair already covers the leak side; this accessor
// closes the "is the sink up?" half so /health doesn't have to peek at
// internal atomics.
//
// Safe to call from any goroutine. Returns false on a nil receiver so
// /health request paths that observe a torn-down EventLog (rare, but
// possible during shutdown) report "not ready" rather than panic.
func (l *EventLog) SinkReady() bool {
	if l == nil {
		return false
	}
	return l.sinkReady.Load()
}

// stampUUID guarantees every appended EventEntry has a non-empty
// UUID. Legacy callers that already set UUID (e.g. history replay
// paths using textutil.DeriveLegacyUUID for determinism) keep their
// value; everything else gets a fresh newEventUUID.
//
// Called from Append / AppendBatch inside the l.mu write-lock so
// the ring buffer always stores the definitive UUID downstream
// readers (Entries, EntriesSince, EntriesBefore, invokePersistSink)
// see.
func stampUUID(e *EventEntry) {
	if e.UUID == "" {
		e.UUID = newEventUUID()
	}
}

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]EventEntry, maxSize)}
}

// pendingTaskDone captures a task_done callback invocation that
// applyEntryStateLocked wants to run *after* the caller has released l.mu.
// Deferring the dispatch keeps Append / AppendBatch's "one lock acquisition
// per call" contract intact — firing inline and re-acquiring would let a
// concurrent Append slip between batch entries and interleave ring-buffer
// writes. R201-CRIT-1.
type pendingTaskDone struct {
	TaskID string
	Status string
}

// applyEntryStateLocked updates per-turn agent tracking for a single entry.
// Caller MUST hold l.mu. Summary atomic writes are the caller's responsibility
// so that AppendBatch can coalesce multiple per-type updates into one Store.
//
// Returns (true, pending) when the entry is a "task_done" event that warrants
// an external callback dispatch; callers should accumulate pending patches
// and fire them after releasing l.mu via fireTaskDoneCallbacks.
// entryAffectsAgentState reports whether an entry's Type causes
// applyEntryStateLocked to perform any work. The hot path is dominated
// by `assistant_text` / `tool_use` / `tool_result` / `system` events
// which fall through the switch's default arm with zero work; gating
// the call site on this predicate avoids the O(N) turnAgents/bgAgents
// scans that the default arm would still trigger inside the switch
// dispatch when called per-entry under l.mu (R240-PERF-3 / R240-PERF-2
// — the AppendBatch replay path runs 500-entry InjectHistory bursts
// where typically <5% are agent-state events). The predicate must
// stay in lockstep with applyEntryStateLocked's case labels.
func entryAffectsAgentState(t string) bool {
	switch t {
	case "agent", "task_start", "task_progress", "task_done", "result", "user":
		return true
	}
	return false
}

func (l *EventLog) applyEntryStateLocked(e EventEntry) (fire bool, pending pendingTaskDone) {
	switch e.Type {
	case "agent":
		label := e.Subagent
		if label == "" {
			label = e.TeamName
		}
		if label == "" {
			label = "agent"
		}
		info := SubagentInfo{
			Name:       label,
			Activity:   e.Summary,
			Background: e.Background,
			ToolUseID:  e.ToolUseID,
			TaskType:   e.TaskType,
			Status:     "spawned",
		}
		if e.Background {
			l.bgAgents = append(l.bgAgents, info)
		} else {
			l.turnAgents = append(l.turnAgents, info)
		}
		l.turnAgentCount.Store(int32(len(l.turnAgents) + len(l.bgAgents)))
	case "task_start":
		// task_started arrives 0-200ms after the "agent" tool_use. Match
		// by ToolUseID (authoritative; Agent tool_use → system.task_started
		// carries the same id). RFC §3.2 deliberately skips InternalAgentID
		// here — SubagentLinker.Resolve is async and fills it via
		// SetAgentInternalID below once the on-disk jsonl is located.
		for i := range l.turnAgents {
			if l.turnAgents[i].ToolUseID != "" && l.turnAgents[i].ToolUseID == e.ToolUseID {
				l.turnAgents[i].TaskID = e.TaskID
				l.turnAgents[i].Status = "running"
				l.turnAgents[i].StartedAtMS = e.Time
				return false, pendingTaskDone{}
			}
		}
		for i := range l.bgAgents {
			if l.bgAgents[i].ToolUseID != "" && l.bgAgents[i].ToolUseID == e.ToolUseID {
				l.bgAgents[i].TaskID = e.TaskID
				l.bgAgents[i].Status = "running"
				l.bgAgents[i].StartedAtMS = e.Time
				return false, pendingTaskDone{}
			}
		}
	case "task_progress":
		// Update live counters from the parent stream. Aggregator in
		// agent_tailer.go may also push meta, but the parent stream is
		// authoritative for totals when present.
		for i := range l.turnAgents {
			if l.turnAgents[i].TaskID != "" && l.turnAgents[i].TaskID == e.TaskID {
				if e.LastTool != "" {
					l.turnAgents[i].LastTool = e.LastTool
				}
				if e.ToolUses > 0 {
					l.turnAgents[i].ToolUses = e.ToolUses
				}
				if e.DurationMS > 0 {
					l.turnAgents[i].DurationMS = e.DurationMS
				}
				return false, pendingTaskDone{}
			}
		}
		// task_started/task_done both consult bgAgents on miss; mirror
		// that fallback here so background-agent progress events are
		// not silently dropped on the floor.
		for i := range l.bgAgents {
			if l.bgAgents[i].TaskID != "" && l.bgAgents[i].TaskID == e.TaskID {
				if e.LastTool != "" {
					l.bgAgents[i].LastTool = e.LastTool
				}
				if e.ToolUses > 0 {
					l.bgAgents[i].ToolUses = e.ToolUses
				}
				if e.DurationMS > 0 {
					l.bgAgents[i].DurationMS = e.DurationMS
				}
				return false, pendingTaskDone{}
			}
		}
	case "task_done":
		status := e.Status
		if status == "" {
			status = "completed"
		}
		matched := false
		for i := range l.turnAgents {
			if l.turnAgents[i].TaskID != "" && l.turnAgents[i].TaskID == e.TaskID {
				l.turnAgents[i].Status = status
				if e.DurationMS > 0 {
					l.turnAgents[i].DurationMS = e.DurationMS
				}
				if e.ToolUses > 0 {
					l.turnAgents[i].ToolUses = e.ToolUses
				}
				matched = true
				break
			}
		}
		if !matched {
			for i := range l.bgAgents {
				if l.bgAgents[i].TaskID != "" && l.bgAgents[i].TaskID == e.TaskID {
					l.bgAgents[i].Status = status
					if e.DurationMS > 0 {
						l.bgAgents[i].DurationMS = e.DurationMS
					}
					break
				}
			}
		}
		if e.TaskID != "" {
			return true, pendingTaskDone{TaskID: e.TaskID, Status: status}
		}
		return false, pendingTaskDone{}
	case "result", "user":
		// R230-PERF-5: a turn that spawned dozens of subagents (e.g. a
		// TeamCreate fan-out) inflates the backing array; subsequent
		// SnapshotTurnAgents copies pay len*sizeof on every Snapshot even
		// when the live count is zero. Drop the array when it grew past a
		// typical-turn threshold so the next turn re-grows from scratch.
		const subagentTurnRetainCap = 8
		if cap(l.turnAgents) > subagentTurnRetainCap {
			l.turnAgents = nil
		} else {
			l.turnAgents = l.turnAgents[:0]
		}
		if cap(l.bgAgents) > subagentTurnRetainCap {
			l.bgAgents = nil
		} else {
			l.bgAgents = l.bgAgents[:0]
		}
		// Most non-agent turns leave turnAgentCount at zero already;
		// skipping the redundant atomic Store avoids cache-coherence
		// traffic on every result event in agent-free workloads.
		// (R227-PERF-14)
		if l.turnAgentCount.Load() != 0 {
			l.turnAgentCount.Store(0)
		}
	}
	return false, pendingTaskDone{}
}

// SetOnAgentTaskDone installs a callback that fires when a "task_done"
// EventEntry is appended. Atomic store — multiple subscribers are
// forbidden (setting a second time replaces the first). Used by the
// server-side tailer registry to stop tailers promptly once the parent
// stream marks an agent task finished. nil clears.
func (l *EventLog) SetOnAgentTaskDone(fn func(taskID, status string)) {
	if fn == nil {
		l.onAgentTaskDoneFn.Store(nil)
		return
	}
	l.onAgentTaskDoneFn.Store(&fn)
}

// loadAgentTaskDoneFn returns the current on-task-done callback so the
// dispatch loops (single + batch) below can read it without taking a
// lock. Returns nil when no callback is wired — callers must treat
// that as a no-op. R233B-PERF-6.
func (l *EventLog) loadAgentTaskDoneFn() func(taskID, status string) {
	if p := l.onAgentTaskDoneFn.Load(); p != nil {
		return *p
	}
	return nil
}

// fireTaskDoneCallbacks dispatches previously-collected task_done callbacks
// outside l.mu. Append/AppendBatch accumulate pendingTaskDone entries while
// holding l.mu, release the lock cleanly, and then call this helper — so a
// slow callback (e.g. tailer registry closing 50 tailers) cannot block
// concurrent Appends or interleave ring-buffer writes. R201-CRIT-1.
//
// Safe to call with an empty slice; common case on non-task_done appends.
func (l *EventLog) fireTaskDoneCallbacks(pending []pendingTaskDone) {
	if len(pending) == 0 {
		return
	}
	fn := l.loadAgentTaskDoneFn()
	if fn == nil {
		return
	}
	for _, p := range pending {
		fn(p.TaskID, p.Status)
	}
}

// fireOneTaskDoneCallback is the single-entry fast path used by Append's
// hot path to avoid a one-slot slice literal escape. Append observes at
// most one pending task_done per call (a single Event maps to one
// EventEntry), so the batch-shaped helper above is unnecessary overhead
// here. AppendBatch keeps using the slice variant because it accumulates
// across multi-entry batches. R224-PERF-1 / R232-CR-16.
func (l *EventLog) fireOneTaskDoneCallback(pending pendingTaskDone) {
	fn := l.loadAgentTaskDoneFn()
	if fn == nil {
		return
	}
	fn(pending.TaskID, pending.Status)
}

// SetAgentInternalID writes the SubagentLinker-resolved linkage back into
// the most recent matching "agent" / "task_start" EventEntry and the live
// SubagentInfo. Called from the Linker's OnResolve callback.
//
// All four fields are written together so persistHistory's next flush captures
// a self-contained record that SeedFromHistory can re-consume on restart
// (RFC v4 §3.3.7). Idempotent: repeated calls with the same values are no-ops;
// distinct internal_agent_id for the same tool_use_id overwrites (Resolve
// should never produce divergent values for the same tool_use_id, but the
// guard keeps the state machine simple if it ever does).
func (l *EventLog) SetAgentInternalID(toolUseID, internalAgentID, jsonlPath, firstPromptID string) {
	if toolUseID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Backfill live SubagentInfo first (hot read path for Snapshot).
	for i := range l.turnAgents {
		if l.turnAgents[i].ToolUseID == toolUseID {
			l.turnAgents[i].InternalAgentID = internalAgentID
			break
		}
	}
	for i := range l.bgAgents {
		if l.bgAgents[i].ToolUseID == toolUseID {
			l.bgAgents[i].InternalAgentID = internalAgentID
			break
		}
	}

	// Backfill ring-buffer entries so future persistHistory / Entries /
	// EntriesSince reads carry the linkage. Walk backwards — the matching
	// "agent" and "task_start" entries are almost always among the last N
	// entries (single turn). R225-PERF-13: cap the scan depth at
	// setAgentInternalIDMaxScan and break once both expected entries (one
	// "agent" + one "task_start" with this ToolUseID) have been backfilled,
	// so the wlock isn't held for an O(maxSize) scan across all 500
	// ring-buffer slots while every Append call is queued behind it.
	start := (l.head - l.count + l.maxSize) % l.maxSize
	scanLimit := l.count
	if scanLimit > setAgentInternalIDMaxScan {
		scanLimit = setAgentInternalIDMaxScan
	}
	var foundAgent, foundTaskStart bool
	for i := 0; i < scanLimit; i++ {
		idx := (start + l.count - 1 - i) % l.maxSize
		e := &l.entries[idx]
		if e.ToolUseID != toolUseID {
			continue
		}
		switch e.Type {
		case "agent":
			if foundAgent {
				continue
			}
			foundAgent = true
		case "task_start":
			if foundTaskStart {
				continue
			}
			foundTaskStart = true
		default:
			continue
		}
		e.InternalAgentID = internalAgentID
		e.JSONLPath = jsonlPath
		e.FirstPromptID = firstPromptID
		if foundAgent && foundTaskStart {
			break
		}
	}
}

// Append adds an entry to the log, overwriting the oldest entry when full.
// Signals all subscribers non-blockingly after appending.
func (l *EventLog) Append(e EventEntry) {
	// R225-PERF-11: stamp UUID *before* taking l.mu — newEventUUID calls
	// crypto/rand.Read which on Linux is a getrandom() syscall. Holding l.mu
	// across that syscall serialises every concurrent Append behind the
	// kernel entropy pool and bloats lock-hold time on the 5-50 events/s
	// per-session hot path. stampUUID is a pure function of the entry's UUID
	// field — caller-set UUIDs (history replay paths) are preserved unchanged.
	stampUUID(&e)
	l.mu.Lock()
	// Single time.Now() feeds both the event timestamp (if absent) and the
	// lastEventAt heartbeat below. Both reads used to happen independently
	// causing two vDSO calls per Append on the hot path. The tiny skew
	// between the two was meaningless — Cleanup only needs "some event
	// landed recently", and the entry's own Time already represents the
	// "actually received at" moment.
	now := time.Now()
	if e.Time == 0 {
		e.Time = now.UnixMilli()
	}
	// Server-side enforcement that every image entry is a data:image/* URI.
	// Today's sole producer (MakeThumbnail) already conforms, but enforcing
	// the contract here rather than trusting callers means any future
	// producer that accidentally passes through an external URL or a
	// javascript: URI gets stripped before it can reach the dashboard's
	// <img src=...> render path. S15 (Round 174).
	//
	// Fast-path skip: 99%+ of events carry no images; hoist the len check
	// to avoid the function call overhead on every live append.
	if len(e.Images) > 0 {
		e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
	}
	l.entries[l.head] = e
	l.head = (l.head + 1) % l.maxSize
	if l.count < l.maxSize {
		l.count++
	}

	// Skip the switch dispatch + per-call frame for entry types that
	// fall through applyEntryStateLocked's default arm with zero work
	// (assistant_text, tool_use, tool_result, system, …). R240-PERF-3.
	var (
		firePending bool
		pending     pendingTaskDone
	)
	if entryAffectsAgentState(e.Type) {
		firePending, pending = l.applyEntryStateLocked(e)
	}

	// Atomic summary stores are issued *inside* l.mu so that AppendBatch,
	// which holds l.mu for its full duration, cannot have its later Store
	// racing with a concurrent live Append's Store — the serialization on
	// l.mu guarantees last-writer-wins matches entry-order, not
	// entry-ordering-inverted by lock release scheduling.
	if e.Type == "user" {
		storeAtomicString(&l.lastPromptSummary, e.Summary)
		l.userTurnCount.Add(1)
	} else if e.Type == "text" {
		// R110-P1: assistant text reply — feed sidebar 30-rune preview.
		// Stored even when Summary is empty so a freshly-streamed empty
		// text block (rare but possible) overwrites stale values rather
		// than leaving last-turn's response visible.
		storeAtomicString(&l.lastResponseSummary, e.Summary)
	} else if IsActivityType(e.Type) {
		// IsActivityType is the shared predicate for the "activity" set;
		// session.ManagedSession history scans also consume it so the
		// live-tail and replay-tail stay aligned. R228-CR-3.
		storeAtomicString(&l.lastActivitySummary, e.Summary)
	}

	// Record live-activity timestamp. A single Store is fine: Cleanup only
	// cares about "some event landed recently", and later Appends overwrite
	// with a never-decreasing value. Reuses the `now` captured at function
	// entry — one vDSO call per Append instead of two.
	l.lastEventAt.Store(now.UnixNano())

	l.mu.Unlock()

	// Fire task_done callbacks OUTSIDE l.mu so a slow subscriber (e.g. the
	// server tailer registry closing N tailers) cannot serialise concurrent
	// Append calls or wedge the ring buffer mid-write. R201-CRIT-1.
	//
	// R224-PERF-1: use the single-entry fast path so the one-slot slice
	// literal `[]pendingTaskDone{pending}` does not heap-escape on every
	// task_done event. AppendBatch still calls the slice form below.
	if firePending {
		l.fireOneTaskDoneCallback(pending)
	}

	// Invoke persistence sink OUTSIDE l.mu. Passing a fresh one-slot
	// slice matches PersistSink's retention contract (callers may hold
	// the slice past return). The slice copy is O(1) because len=1.
	//
	// R230-PERF-1: skip the slice literal entirely when no sink is
	// wired (test harnesses, headless tools, the InjectHistory replay
	// phase before the persister attaches). The slice header + the
	// `EventEntry` copy together heap-escape on every Append; bypassing
	// them in the no-sink case saves one alloc per event in the hot
	// stdout path. Mirrors AppendBatch's pre-loop sinkAttached gate.
	//
	// #410: prefer the single-entry sink when the caller paired one via
	// SetPersistSinkPair. The single sink path passes the EventEntry by
	// value, eliminating the `[]EventEntry{e}` literal that would
	// otherwise heap-escape through the slice sink's retention contract.
	// Falls back to the slice form for legacy SetPersistSink-only
	// callers; both branches share replayPhase derivation +
	// replayInvokeTotal accounting through invokePersistSinkOne /
	// invokePersistSink.
	//
	// R215-PERF-P2-1 / R219-PERF-4 / R228-PERF-7 archive anchor:
	// the slice-literal allocation on the legacy sink-attached branch
	// remains structurally required by PersistSink's retention
	// contract — opting into SetPersistSinkPair is the way to skip it.
	if !l.invokePersistSinkOne(e) {
		if l.persistSinkPtr.Load() != nil {
			l.invokePersistSink([]EventEntry{e})
		}
	}

	l.notifySubscribers()
}

// AppendBatch adds multiple entries to the log, holding the lock once and
// notifying subscribers once. Used by live dispatch (multi-block assistant
// events) to avoid per-entry lock acquisition + subscriber wake-ups.
//
// Mirrors Append's per-entry sub-agent tracking and summary atomics so the
// sidebar does not show stale "(no prompt)" placeholders after history
// injection until a live event arrives. Atomic summary writes happen under
// l.mu to avoid a race with concurrent Append: if a live event ran Store
// after our Unlock but before our own Store, our older batch value would
// clobber it.
func (l *EventLog) AppendBatch(entries []EventEntry) {
	l.appendBatch(entries, false)
}

// AppendBatchReplay is the replay-aware variant used by InjectHistory.
// Setting isReplay=true skips applyEntryStateLocked entirely: replay never
// triggers the on-task-done callback (the persister is not yet wired and
// downstream tailers don't yet exist), so the per-entry switch dispatch +
// turn/bg agent slice scans inside l.mu are pure overhead. R240-PERF-3
// (#1042). Live AppendBatch callers MUST keep isReplay=false so task_done
// callbacks continue to fire on real turn-end events.
func (l *EventLog) AppendBatchReplay(entries []EventEntry) {
	l.appendBatch(entries, true)
}

func (l *EventLog) appendBatch(entries []EventEntry, isReplay bool) {
	if len(entries) == 0 {
		return
	}
	var (
		lastPrompt, lastActivity, lastResponse string
		sawPrompt, sawActivity, sawResponse    bool
		userDelta                              int64
		pendingDone                            []pendingTaskDone
	)
	// Capture a single wall-clock read before locking so the N zero-time
	// entries inside the loop (typical case: InjectHistory's 500-entry
	// replay on shim reconnect) don't each fire a vDSO call under l.mu.
	// Correctness: entries with an explicit Time are unaffected; entries
	// without one are assigned a monotonically-close "now" that is as
	// semantically correct as the per-entry reads they replace, while
	// keeping the write-lock hold time bounded. R71-PERF-L2.
	defaultTime := time.Now().UnixMilli()
	// Allocate the sink-copy slice outside the lock so the write
	// lock hold time is bounded by the ring write itself. The slice
	// is populated inside the loop and handed to invokePersistSink
	// after unlock.
	//
	// Fast path: when no persist sink is wired we skip the per-batch
	// allocation entirely. invokePersistSink does the same nil check at
	// :337 but only after we've already paid for the slice; routers
	// without a sink (test harnesses, headless tools, the InjectHistory
	// replay path before the persister is attached) hit this branch on
	// every batch and a 500-slot allocation per replay adds up. Read the
	// sink pointer once here so the body and the post-unlock dispatch
	// agree on whether to capture; a Store racing this read is fine —
	// the late-attached sink will pick up subsequent batches and the
	// missed ones are bounded by the same replayPhase contract that
	// already gates the early append phase.
	// R242-PERF-8: skip the sinkCopy allocation entirely when the sink
	// observes !sinkReady (replay phase). The persister sink unconditionally
	// drops replay-phase batches (see Persister.SinkFor → replayLeakCnt path),
	// so capturing entries we know will be discarded just burns heap on every
	// InjectHistory's 500-entry replay round-trip.
	//
	// Read order: ptr first, then sinkReady. If a SetPersistSink races between
	// our two loads we'll observe (ptr=non-nil, ready=false) → still skip;
	// the batch is genuinely replayPhase from the contract's POV because
	// SetPersistSink writes ptr first and ready second. The next batch after
	// SetPersistSink completes will see ready=true and allocate normally.
	//
	// `sinkAttached` covers the historical fast-path (no sink wired at all,
	// e.g. test harnesses); `captureForSink` is the per-batch decision used
	// to gate both the allocation above and the in-loop append below — they
	// must agree, otherwise the loop would append into a nil slice or the
	// post-unlock dispatch would receive an empty copy from a non-replay
	// batch.
	sinkAttached := l.persistSinkPtr.Load() != nil
	captureForSink := sinkAttached && l.sinkReady.Load()
	// R225-PERF-11 + R249-PERF-16: single pre-lock pass that stamps UUIDs,
	// applies the default Time, and sanitises image URIs. The N
	// crypto/rand.Read syscalls (getrandom, one per missing UUID) and the
	// ~200KB sinkCopy build for a 500-entry InjectHistory replay all happen
	// outside the write-lock. Caller-set UUIDs (history replay) are preserved
	// by stampUUID's no-op-on-non-empty contract.
	//
	// `sinkCopy` doubles as the inner-loop iteration source so per-entry
	// stamping (default time, image sanitize) is also paid only once per
	// entry — the ring-buffer write inside the lock simply assigns the
	// pre-prepared struct without re-running sanitize/default-time logic.
	//
	// Fast path (!captureForSink): sinkCopy stays nil and the inner loop
	// falls back to the historical "stamp inside lock" path. Test harnesses
	// and the InjectHistory phase before the persister attaches don't pay
	// for the extra 200KB allocation, but UUID stamping still runs here.
	var sinkCopy []EventEntry
	if captureForSink {
		sinkCopy = make([]EventEntry, len(entries))
	}
	for i := range entries {
		stampUUID(&entries[i])
		if !captureForSink {
			continue
		}
		e := entries[i]
		if e.Time == 0 {
			e.Time = defaultTime
		}
		// S15 (Round 174): same enforcement as Append. Replays from
		// history (InjectHistory → AppendBatch) should never contain
		// non-image data URIs today, but defense-in-depth is trivially
		// cheap and locks the contract to a single sink.
		if len(e.Images) > 0 {
			e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
		}
		sinkCopy[i] = e
	}
	l.mu.Lock()
	for idx, e := range entries {
		if captureForSink {
			// Already prepared above; use the sink copy as the source of
			// truth so the ring-buffer entry matches what the persister
			// will write. Avoids a divergence window where Time / Images
			// could differ between in-memory ring and on-disk record.
			e = sinkCopy[idx]
		} else {
			if e.Time == 0 {
				e.Time = defaultTime
			}
			if len(e.Images) > 0 {
				e.Images, e.ImagePaths = sanitizeImagesAligned(e.Images, e.ImagePaths)
			}
		}
		l.entries[l.head] = e
		l.head = (l.head + 1) % l.maxSize
		if l.count < l.maxSize {
			l.count++
		}

		// Skip applyEntryStateLocked for entries whose Type is not one of
		// the 6 cases the function actually handles. InjectHistory's
		// 500-entry replay is dominated by assistant_text/tool_use rows
		// which previously paid switch-dispatch + return overhead inside
		// the write lock. R240-PERF-3.
		//
		// On the replay path we skip applyEntryStateLocked unconditionally:
		// no on-task-done subscriber is wired during InjectHistory (#1042),
		// and the turnAgents/bgAgents per-turn slices are reset by the
		// next live "result"/"user" event anyway. This avoids 500× O(N)
		// agent-slice scans inside the write-lock during shim reconnect.
		if !isReplay && entryAffectsAgentState(e.Type) {
			if fire, p := l.applyEntryStateLocked(e); fire {
				pendingDone = append(pendingDone, p)
			}
		}

		// Track last-of-kind summaries so a single Store (below, still
		// under l.mu) captures the tail of the batch. The "saw" flag is
		// separate from the value so an empty final Summary still
		// overwrites the atomic — Append stores unconditionally for these
		// types, and diverging here would leave stale summaries visible.
		if e.Type == "user" {
			lastPrompt = e.Summary
			sawPrompt = true
			userDelta++
		} else if e.Type == "text" {
			// R110-P1: track tail assistant text for sidebar response preview.
			// Mirrors the live Append store under l.mu — last-writer-wins
			// matches entry order even when batches interleave with live
			// Appends. See lastPromptSummary single-Store treatment above.
			lastResponse = e.Summary
			sawResponse = true
		} else if IsActivityType(e.Type) {
			lastActivity = e.Summary
			sawActivity = true
		}
	}

	if sawPrompt {
		storeAtomicString(&l.lastPromptSummary, lastPrompt)
	}
	if sawResponse {
		storeAtomicString(&l.lastResponseSummary, lastResponse)
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

	l.fireTaskDoneCallbacks(pendingDone)

	// Invoke persistence sink outside l.mu. sinkCopy holds the
	// post-stamp, post-sanitize entries in the SAME order they were
	// committed to the ring buffer — critical for the Persister's
	// write-order guarantees.
	l.invokePersistSink(sinkCopy)

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
//
// R239-PERF-9 (2026-05-24): subscribers storage migrated from
// map[*subscriber]struct{} to []*subscriber. The hot iter dropped
// mapiterinit/mapiternext (~tens of ns/call × 25K calls/s = measurable
// in 500-session deployments) for a tight slice range. Unsubscribe is
// the cold path (one alloc/subscribe per session lifetime) and pays
// an O(N) scan to find + swap-to-end the leaving subscriber. closeOnce
// on subscriber.ch keeps the "close exactly once" invariant safe across
// the unsub-vs-CloseSubscribers race.
func (l *EventLog) notifySubscribers() {
	if l.subCount.Load() == 0 {
		return
	}
	l.subMu.RLock()
	for _, sub := range l.subscribers {
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
		// R230C-PERF-12 / R239-PERF-9: pre-size the slice. CloseSubscribers
		// nils out the slice so each Subscribe after a teardown allocates
		// a fresh backing array; without a cap hint Go would grow
		// 1 → 2 → 4 → 8 across a typical dashboard reconnect spurt (one
		// tab subscribes 4–6 sessions back-to-back). 4 covers the common
		// case in a single allocation; the slice still grows naturally
		// when the per-session subscriber count climbs (multi-tab
		// dashboards, agent_tailer fan-in).
		l.subscribers = make([]*subscriber, 0, 4)
	}
	l.subscribers = append(l.subscribers, sub)
	// Add/sub counter pattern rather than re-deriving from len(map) — avoids
	// the map-header read on each mutation and makes the reader/writer
	// asymmetry explicit (Load is on the hot notify path, Add is rare).
	// R65-PERF-L-4.
	l.subCount.Add(1)
	l.subMu.Unlock()

	unsub := func() {
		l.subMu.Lock()
		// Linear scan + swap-to-end + truncate. Subscribers count is
		// typically 1-10 (one per dashboard tab subscribed to this
		// session), so this is O(N) on the cold unsubscribe path. The
		// hot notifySubscribers path benefits in exchange. R239-PERF-9.
		for i, s := range l.subscribers {
			if s == sub {
				last := len(l.subscribers) - 1
				l.subscribers[i] = l.subscribers[last]
				// Clear the trailing slot so the removed *subscriber
				// is not retained by the backing array; otherwise a
				// long-lived EventLog holds onto closed subscriber
				// objects past their useful life.
				l.subscribers[last] = nil
				l.subscribers = l.subscribers[:last]
				l.subCount.Add(-1)
				break
			}
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
	for _, sub := range l.subscribers {
		sub.closeOnce.Do(func() { close(sub.ch) })
	}
	l.subscribers = nil
	l.subCount.Store(0)
	l.subsClosed = true
}

// Entries returns a copy of all entries in chronological order.
//
// Uses defer RUnlock: a panic during make([]EventEntry, l.count) (e.g. OOM
// for very large rings) would otherwise leave the lock permanently held and
// deadlock subsequent writers. The defer cost is a handful of ns and not
// material on the broadcast fan-out path.
//
// R247-PERF-13 / R246-PERF-16 [REPEAT-3]: Entries() allocates a fresh
// `[]EventEntry` of up to `maxSize=500` slots (~140KB) on every call, and the
// dashboard subscribe path on a 500-session deployment hits this in steady
// state. Callers that re-fetch the whole log on a hot loop (dashboard 1Hz
// poll, agent_tailer fan-in) should prefer `LastN(n)` with a bounded `n` to
// keep the working set small, or use `EntriesAppend(dst)` to recycle a
// caller-owned backing array via sync.Pool. Entries() is retained as the
// "give me everything" convenience used by tests and one-shot history dumps;
// the documented expectation is that production hot paths bound their reads.
func (l *EventLog) Entries() []EventEntry {
	return l.LastNAppend(nil, 0)
}

// LastN returns the most recent n entries in chronological order.
// If n <= 0 or n >= count, all entries are returned.
//
// Uses defer RUnlock; see Entries for rationale. Backing array pooled —
// see Entries godoc for the lifetime contract.
func (l *EventLog) LastN(n int) []EventEntry {
	return l.LastNAppend(nil, n)
}

// EntriesAppend copies all entries in chronological order into `dst`,
// reslicing it (and growing the backing array if cap is short). When
// `dst` already has enough capacity (e.g. retrieved from a sync.Pool
// of pre-grown buffers), no allocation occurs on the hot path.
//
// Pass dst[:0] when reusing a pooled buffer; passing nil is equivalent
// to Entries() (allocates a fresh slice sized exactly to l.count).
//
// R247-PERF-13: callers on the dashboard fan-out path (poll-style
// refresh on every WS notify) can amortise the per-call ~140KB
// allocation by holding a sync.Pool of `[]EventEntry` and rotating
// the slice through this method. Lifetime contract: the returned
// slice is fully owned by the caller after the call returns; the
// EventLog never retains a reference. Callers that route the slice
// onto a channel must NOT recycle it until the consumer signals
// completion — standard pool-of-slice discipline.
func (l *EventLog) EntriesAppend(dst []EventEntry) []EventEntry {
	return l.LastNAppend(dst, 0)
}

// LastNAppend is the buffer-reusing variant of LastN. See EntriesAppend
// for the lifetime contract; pass `n<=0` for "all entries" semantics.
func (l *EventLog) LastNAppend(dst []EventEntry, n int) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.count
	if n > 0 && n < count {
		count = n
	}
	if cap(dst) >= count {
		dst = dst[:count]
	} else {
		dst = make([]EventEntry, count)
	}
	start := (l.head - count + l.maxSize) % l.maxSize
	for i := 0; i < count; i++ {
		dst[i] = l.entries[(start+i)%l.maxSize]
	}
	return dst
}

// Count returns the current number of valid entries (0..maxSize).
// Useful for sync.Pool-backed callers that want to right-size their
// scratch buffer before a LastNAppend call.
func (l *EventLog) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.count
}

// EntriesSince returns entries after the given unix ms timestamp, in chronological order.
// Single-pass backward scan collects matches into a reverse buffer; the caller
// receives them in chronological order. Previous implementation did two passes
// (count, then copy forward), touching each matched ring slot twice. For the
// hot streaming path (k = 1-5 new events per notify) the constant savings are
// small but the code path is simpler and avoids the arithmetic error surface
// of two separate modular indexing expressions.
//
// R220-PERF-3 (#685): the in-place slices.Reverse runs OUTSIDE l.mu so an
// initial subscribe with hundreds of matched entries cannot block a
// concurrent Append. The reverse-buffer (`rev`) holds value-copied
// EventEntry slots; once the scan returns we no longer touch l.entries
// and the lock can be released. RUnlock is explicit (not deferred) so
// the reverse touches the locally-owned slice only, shrinking the
// reader-blocks-writer footprint from "RLock for the whole function"
// to "RLock for the scan only".
func (l *EventLog) EntriesSince(afterMS int64) []EventEntry {
	l.mu.RLock()
	if l.count == 0 {
		l.mu.RUnlock()
		return nil
	}
	// First pass: collect matches in reverse order. Most calls match 0-5
	// entries so we allocate lazily only when the first match is found.
	//
	// R249-PERF-17: hoist the modulo arithmetic out of the loop.
	// Previously each iter recomputed `(l.head - l.count + i + l.maxSize) % l.maxSize`
	// — a DIV per step. Walk backward from the newest slot with a cheap
	// branch-on-wrap instead. ~5-10ns × notify wave on hot streaming path.
	var rev []EventEntry
	idx := l.head - 1
	if idx < 0 {
		idx += l.maxSize
	}
	for i := l.count - 1; i >= 0; i-- {
		if l.entries[idx].Time <= afterMS {
			break
		}
		if rev == nil {
			// Typical streaming match count is 1-5; cap at entriesSinceInitialCap
			// so sessions with hundreds of buffered entries don't allocate a
			// giant backing array on every notify. `append` will grow organically
			// if the match count exceeds this hint.
			initialCap := l.count - i
			if initialCap > entriesSinceInitialCap {
				initialCap = entriesSinceInitialCap
			}
			rev = make([]EventEntry, 0, initialCap)
		}
		rev = append(rev, l.entries[idx])
		idx--
		if idx < 0 {
			idx += l.maxSize
		}
	}
	l.mu.RUnlock()
	if len(rev) == 0 {
		return nil
	}
	slices.Reverse(rev)
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
	// R249-PERF-17: walk backward with hoisted index instead of recomputing
	// (l.head - l.count + i + l.maxSize) % l.maxSize per iter. Same shape
	// as EntriesSince — branch-on-wrap is one CMOV/cmp vs an IDIV.
	var rev []EventEntry
	crossed := beforeMS <= 0 // when beforeMS==0 treat as "no upper bound"
	idx := l.head - 1
	if idx < 0 {
		idx += l.maxSize
	}
	for i := l.count - 1; i >= 0 && len(rev) < limit; i-- {
		if !crossed {
			if l.entries[idx].Time >= beforeMS {
				idx--
				if idx < 0 {
					idx += l.maxSize
				}
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
		idx--
		if idx < 0 {
			idx += l.maxSize
		}
	}
	if len(rev) == 0 {
		return nil
	}
	slices.Reverse(rev)
	return rev
}

// LastPromptSummary returns the summary of the most recent "user" entry.
func (l *EventLog) LastPromptSummary() string {
	return loadAtomicString(&l.lastPromptSummary)
}

// LastActivitySummary returns the summary of the most recent "tool_use" or "thinking" entry.
func (l *EventLog) LastActivitySummary() string {
	return loadAtomicString(&l.lastActivitySummary)
}

// LastResponseSummary returns the summary of the most recent assistant "text"
// entry. Used by the sidebar to render a 30-rune dim preview line under the
// prompt (R110-P1). Empty when no assistant text has streamed yet.
func (l *EventLog) LastResponseSummary() string {
	return loadAtomicString(&l.lastResponseSummary)
}

// LastEventAt returns the wall-clock time of the most recent live Append,
// or the zero Time when no live event has been appended yet (only
// InjectHistory / AppendBatch replays, or a freshly spawned log).
// Consumed by Router.Cleanup to avoid misclassifying a long-running but
// actively streaming turn as a stuck session. Lock-free.
func (l *EventLog) LastEventAt() time.Time {
	ns := l.lastEventAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// UserTurnCount returns the cumulative count of "user" entries appended to
// this log since the Process was spawned. Consumed by SessionSnapshot.MessageCount
// for sidebar / main-header display. Increments once per Append of a user entry
// and by the batch's user-entry count inside AppendBatch. Ring-buffer eviction
// does not decrement.
func (l *EventLog) UserTurnCount() int64 {
	return l.userTurnCount.Load()
}

// loadAtomicString and storeAtomicString are thin wrappers around the
// shared textutil.LoadAtomicString / textutil.StoreAtomicString helpers
// (R219-CR-1: was a word-for-word copy of session.loadAtomicString /
// storeAtomicString — both word orders inverted before the rename). Kept
// as package-private aliases so the dense Append hot path stays readable
// and call sites do not have to spell out the textutil import path.
// Behavioural contract — fast-path short-circuit on equal value,
// last-writer-wins under l.mu — is documented on the textutil helpers;
// do not re-document the rationale here to keep the two in sync.
//
// R215-PERF-P2-4 archive anchor: the `new(string)` heap alloc on actual
// change is structurally required by atomic.Pointer[string] — Pointer.Store
// needs an addressable string slot. The textutil.StoreAtomicString
// fast-path skips the alloc when the value is unchanged, which covers the
// common steady-state case (same prompt summary repeated). On real
// change there is no zero-alloc atomic-string solution short of moving
// to atomic.Value (which has comparable cost) or a uintptr+intern-table
// scheme (much larger refactor for marginal gain on a low-frequency path
// — turn boundaries, not per stdout line).
func loadAtomicString(v *atomic.Pointer[string]) string {
	return textutil.LoadAtomicString(v)
}

func storeAtomicString(v *atomic.Pointer[string], s string) {
	textutil.StoreAtomicString(v, s)
}

// TurnAgents returns a copy of all currently active agents (foreground + background)
// in the current turn. Both are cleared on turn boundaries (result/user events).
// Returns nil when no agents are active.
//
// Fast path: most sessions have no active sub-agents at any given time, so
// the atomic turnAgentCount lets Snapshot skip the RLock + 0-length slice
// allocation on the common empty read. R220-PERF-6.
func (l *EventLog) TurnAgents() []SubagentInfo {
	if l.turnAgentCount.Load() == 0 {
		return nil
	}
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

// Subagents returns a copy of foreground turn agents only. Used by dashboard
// snapshot enrichment (server.enrichSnapshot) where banner solo/team rows
// need to stay separated from long-lived [bg] tags. Tests also use this to
// pin per-agent lifecycle state without the foreground/background merge.
func (l *EventLog) Subagents() []SubagentInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.turnAgents) == 0 {
		return nil
	}
	out := make([]SubagentInfo, len(l.turnAgents))
	copy(out, l.turnAgents)
	return out
}

// BgSubagents returns a copy of background (run_in_background) turn agents.
// Symmetric with Subagents — see that method's doc for rationale.
func (l *EventLog) BgSubagents() []SubagentInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.bgAgents) == 0 {
		return nil
	}
	out := make([]SubagentInfo, len(l.bgAgents))
	copy(out, l.bgAgents)
	return out
}
