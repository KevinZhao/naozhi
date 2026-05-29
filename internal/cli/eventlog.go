// Package file group eventlog*.go is the IN-MEMORY ring-buffer leg of the
// three "eventlog" packages identified by R237-ARCH-13 (#610). This file
// (eventlog.go) holds the EventLog struct, constants, the EventEntry alias,
// and NewEventLog; the rest is split by responsibility per
// docs/rfc/eventlog-split.md (ARCH-EVENTLOG-SPLIT):
//   eventlog_append.go    write path (Append / AppendBatch / ring eviction)
//   eventlog_agents.go    per-turn subagent tracking + task_done callbacks
//   eventlog_persist.go   PersistSink contract + replay-phase guard
//   eventlog_subscribe.go subscriber broadcast (subMu)
//   eventlog_query.go     read path (Entries* / Since / Before / summaries)
//
// Disambiguation reminder for reviewers — see the same anchor in cli/doc.go
// and internal/eventlog/persist/doc.go for the full picture:
//
//   - THIS package leg (cli.EventLog) — IN-MEMORY ring buffer + PersistSink
//     contract. Producer of every event. Owns EventEntry / SubagentInfo /
//     PersistSink (the cli-side closure type, distinct from
//     persist.PersistSink).
//   - internal/eventlog/persist — ON-DISK writer consuming from
//     cli.EventLog via the PersistSink closure.
//   - internal/eventlog/schema — wire format types shared by persist +
//     replay readers. Strictly upstream of cli.
//   - internal/history/naozhilog — REPLAY reader for the files persist
//     wrote.
//
// R237-ARCH-13 proposes the long-term rename to
// internal/eventlog/{ring, persist, replay} so package names match
// data-flow positions. Until that lands: do NOT collapse references
// between cli.PersistSink and persist.PersistSink — the bridge in
// internal/session/eventlog_bridge.go is the only place that translates
// between them.

package cli

import (
	"sync"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/cli/clievent"
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

// EventEntry is the simplified event record for the dashboard. The struct
// itself lives in `internal/cli/clievent` (R217-ARCH-3 #626 — diamond
// import break); the alias here keeps every existing call site
// (`cli.EventEntry`) compiling. Future leaf consumers (e.g. discovery)
// should import the leaf pkg directly to avoid pulling in the whole cli
// surface.
type EventEntry = clievent.EventEntry

// EventLog is a thread-safe, bounded event log backed by a ring buffer.
//
// Position in the data flow (R237-ARCH-13): EventLog is the IN-MEMORY
// "ring" leg. Append/AppendBatch are the only producers; subscribers
// (dashboard live-tail, agent_tailer, persist sink) are pure consumers.
// The on-disk persistence layer is internal/eventlog/persist; readers
// of historical data come back through internal/history/naozhilog. See
// the file header above and cli/doc.go for the full three-package map
// — this struct does NOT do disk I/O and MUST NOT grow code paths that
// would force callers to wait on fsync.
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

	// R260528-PERF-6 (#1353): sidecar lookup for applyEntryStateLocked
	// task_progress / task_done O(1) match. Populated on task_start
	// (taskID first becomes known there); cleared on result/user alongside
	// the turnAgents/bgAgents reset. Indexes into either turnAgents
	// (background=false) or bgAgents (background=true). Indexes are stable
	// for a turn because the slices only grow via append between resets.
	// Protected by mu.
	taskIndex map[string]subagentRef

	// R240-PERF-2 (#1041): twin sidecar keyed by ToolUseID, populated on
	// the "agent" Append so a task_start can resolve its slot in O(1)
	// instead of scanning turnAgents+bgAgents. Same lifecycle as
	// taskIndex — reset alongside the slice clear.
	toolUseIndex map[string]subagentRef

	// R260528-PERF-22 (#1360): ring-buffer position sidecar keyed by
	// ToolUseID for the "agent" + "task_start" pair, so SetAgentInternalID
	// can reach the two slots in O(1) instead of scanning the last
	// setAgentInternalIDMaxScan ring slots under wlock per linker resolve.
	// A TeamCreate fan-out spawns 8 subagents and the linker resolves them
	// in serial; under the legacy walk each resolve held wlock long enough
	// to back-pressure concurrent Append/Subscribe traffic. Same lifecycle
	// as toolUseIndex — populated on the agent/task_start Append, reset on
	// result/user alongside the slice clear. Indices may go stale if the
	// ring rotates (rare: maxSize=500 vs typical turn <=200 events), so
	// the consumer re-validates Type+ToolUseID at the indexed slot before
	// writing and falls back to the bounded scan on miss.
	agentRingByToolUse map[string]agentRingPos

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

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]EventEntry, maxSize)}
}
