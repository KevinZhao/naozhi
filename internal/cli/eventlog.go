// File eventlog.go holds the EventLog struct, constants and NewEventLog; the
// method set is split by concern: eventlog_append.go (write path),
// eventlog_agents.go (subagent tracking), eventlog_persist.go (PersistSink),
// eventlog_subscribe.go (subscribers, subMu) and eventlog_query.go (reads).
//
// cli.EventLog is the in-memory ring leg of the event pipeline; the on-disk
// writer is internal/eventlog/persist and the replay reader is
// internal/history/naozhilog. cli.PersistSink and persist.PersistSink are
// distinct types; internal/session/eventlog_bridge.go is the only translator.

package cli

import (
	"sync"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

const defaultEventLogSize = 500

// setAgentInternalIDMaxScan caps how many ring entries SetAgentInternalID
// walks backwards for the matching "agent" / "task_start" pair, bounding the
// wlock hold while concurrent Appends queue. An RLock-scan-then-upgrade variant
// is not safe: sync.RWMutex has no atomic upgrade, and RUnlock→Lock lets Append
// rotate the ring head between idx capture and write.
const setAgentInternalIDMaxScan = 50

// entriesSinceInitialCap is the initial capacity of EntriesSince's lazily
// allocated result. Streaming tails call it per notify with 1-5 matches, so
// a small cap avoids a 500-entry array per notify on full rings; append still
// grows for slow consumers catching up.
const entriesSinceInitialCap = 16

// imageDataURIPrefix is the required leading substring for every entry in
// EventEntry.Images. Any producer MUST keep this prefix so the dashboard's
// <img src=...> path cannot be coerced into javascript:, http://evil/ or
// data:text/html payloads.
const imageDataURIPrefix = "data:image/"

// EventLog is a thread-safe, bounded event log backed by a ring buffer.
//
// Append/AppendBatch are the only producers; subscribers (dashboard live-tail,
// agent_tailer, persist sink) are pure consumers. This struct does no disk
// I/O and MUST NOT grow code paths that make callers wait on fsync.
type EventLog struct {
	mu      sync.RWMutex
	entries []clievent.EventEntry // ring buffer, pre-allocated to maxSize
	head    int                   // next write position
	count   int                   // number of valid entries (0..maxSize)
	maxSize int

	// Cached summaries stored atomically under l.mu on Append so AppendBatch's
	// last-writer ordering stays consistent; read lock-free. atomic.Pointer
	// distinguishes never-stored (nil) from a stored empty string.
	lastPromptSummary   atomic.Pointer[string] // most recent "user" entry summary
	lastActivitySummary atomic.Pointer[string] // most recent "tool_use"/"thinking" entry summary
	lastResponseSummary atomic.Pointer[string] // most recent assistant "text" entry summary

	// userTurnCount is the cumulative count of "user" entries since spawn
	// (SessionSnapshot.MessageCount). Replayed entries count too, so
	// InjectHistory rebuilds it after reconnect; ring eviction never
	// decrements it.
	userTurnCount atomic.Int64

	// countAtomic mirrors `count` for lock-free Count(). Updated under l.mu at
	// every `count++` site; `count` is monotonic up to maxSize so Add(1) keeps
	// the mirror exact.
	countAtomic atomic.Int64

	// lastEventAt is the unix-nano time of the most recent live Append. It is
	// Router.Cleanup's second-chance activity signal: lastActive only refreshes
	// on Send entry, so a long turn still streaming events would otherwise be
	// classified stuck and killed. Replays (InjectHistory / recovery) do NOT
	// update it — historical timestamps are not evidence of live activity.
	lastEventAt atomic.Int64

	// Per-turn sub-agent tracking: reset on "result"/"user" events.
	turnAgents []SubagentInfo // foreground agents in current turn; protected by mu
	bgAgents   []SubagentInfo // background (run_in_background) agents; cleared on turn boundaries like turnAgents; protected by mu

	// taskIndex gives applyEntryStateLocked O(1) task_progress / task_done
	// matching. Populated on task_start, cleared with the turnAgents/bgAgents
	// reset; indexes are stable within a turn because the slices only grow.
	// Protected by mu. (#1353)
	taskIndex map[string]subagentRef

	// toolUseIndex is keyed by ToolUseID and populated on the "agent" Append so
	// task_start resolves its slot in O(1). Same lifecycle as taskIndex. (#1041)
	toolUseIndex map[string]subagentRef

	// agentRingByToolUse maps ToolUseID → ring positions of the "agent" and
	// "task_start" pair so SetAgentInternalID avoids the bounded wlock scan.
	// Same lifecycle as toolUseIndex. Indices can go stale if the ring rotates,
	// so the consumer re-validates Type+ToolUseID at the slot and falls back to
	// the scan on miss. (#1360)
	agentRingByToolUse map[string]agentRingPos

	// turnAgentCount mirrors len(turnAgents)+len(bgAgents) for lock-free
	// Snapshot reads (polled at 1Hz × tabs × sessions, usually zero). Updated
	// under l.mu alongside the slice mutations.
	turnAgentCount atomic.Int32

	// onAgentTaskDoneFn fires after a "task_done" entry is ingested, OUTSIDE
	// l.mu so slow subscribers cannot back-pressure Append; callbacks must be
	// fast and re-entrant safe. atomic.Pointer because every Append loads it
	// while writes happen once per session. nil = no subscriber.
	onAgentTaskDoneFn atomic.Pointer[func(taskID, status string)]

	// subMu is an RWMutex: notifySubscribers only reads the slice (iterate +
	// non-blocking send) under RLock so concurrent Appends don't serialise;
	// Subscribe/Unsubscribe/CloseSubscribers take the write lock. A slice beats
	// a map at 1-2 subscribers and ~25K notifies/s; Unsubscribe is O(N)
	// swap-to-end, and subscriber.closeOnce keeps "closed exactly once".
	subMu       sync.RWMutex
	subscribers []*subscriber
	subsClosed  bool         // CloseSubscribers has been called; no new subscribers accepted
	subCount    atomic.Int32 // mirrors len(subscribers); lets notifySubscribers skip the lock when zero

	// Persistence hook. sinkReady starts false: every Append before
	// SetPersistSink carries replayPhase=true so the Persister drops it instead
	// of committing an InjectHistory replay. SetPersistSink stores the pointer
	// then sinkReady=true (order matters, see its godoc); nil = no persistence.
	// ReplayPhase is derived here so the pre-allocated EventEntry stays small.
	sinkReady      atomic.Bool
	persistSinkPtr atomic.Pointer[PersistSink]

	// persistSinkOnePtr is the optional single-entry sink from
	// SetPersistSinkPair; Append prefers it to avoid the heap-escaping
	// `[]EventEntry{e}` literal (#410). AppendBatch always uses persistSinkPtr
	// so the persister sees contiguous batches. nil for legacy SetPersistSink
	// callers.
	persistSinkOnePtr atomic.Pointer[PersistSinkOne]

	// replayInvokeTotal counts sink invocations that fired with sinkReady
	// false. Diagnostic only (/health, tests): a value that keeps growing after
	// SetPersistSink means a caller raced ahead of the persister attach.
	replayInvokeTotal atomic.Int64
}

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]clievent.EventEntry, maxSize)}
}
