package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// Agent tailer layer — streams each team agent's on-disk transcript to the
// dashboard via WebSocket. Lives in the server package because it fans out
// to wsClient connections; the parsing side (cli.TranscriptReader) stays
// backend-agnostic.
//
// Lifecycle (RFC v4 agent-team-ui §3.5.4):
//
//   Linker.OnResolve(taskID, toolUseID, hex) →  registry.ensureTailer()
//     ↓
//   agentTailer (silent): 200ms ticker, Stat+Tail, updates meta + buffers
//     events with no subscriber yet
//     ↓
//   WS agent_subscribe:   registry.attach(client) → refCount++, broadcasting mode
//     (replay buffered events + live push)
//     ↓
//   parent stream task_done: registry.closeTask(taskID) → stop ticker,
//     flush agent_done to all subscribers
//     ↓
//   WS agent_unsubscribe / client disconnect: registry.detach(client) →
//     refCount--, still silent at 0 until task_done or TTL idle.

const (
	// agentTailerPollInterval is the file-stat/Tail cadence. 200ms balances
	// responsiveness against scan cost — 50 active tailers at 200ms = 250 stats/s,
	// tolerable.
	agentTailerPollInterval = 200 * time.Millisecond

	// agentTailerIdleGrace drops a silent tailer (refCount==0) whose file has
	// not grown for this long. Prevents stale tailers accumulating when a
	// team agent finishes quietly and no parent-stream task_done arrives.
	agentTailerIdleGrace = 30 * time.Second

	// agentTailerMax caps concurrent tailers per Hub. Beyond this, new
	// subscribe attempts receive agent_subscribe_rejected{reason:"capacity"}
	// and the dashboard falls back to 3 s HTTP poll (§3.5.4 R FC).
	agentTailerMax = 50
)

// tailerSubsPool / tailerSubsHandle / acquire+release + tailerBufferedPool /
// tailerBufferedHandle / acquire+release moved to agent_tailer_pools.go
// (Phase 4c-prep, 2026-05-28).

// agentTailer streams a single agent jsonl to any number of subscribed
// wsClients and tracks aggregate stats (LastTool/ToolUses/DurationMS) for
// enrichSnapshot consumers even when no client is listening.
type agentTailer struct {
	key       string
	taskID    string
	toolUseID string
	reader    *cli.TranscriptReader
	reg       *tailerRegistry
	hub       *Hub

	stopCh   chan struct{}
	doneOnce sync.Once

	mu         sync.Mutex
	subs       map[*wsClient]struct{}
	refCount   atomic.Int32 // mirrors len(subs); Go 1.19+ idiom (match tailerRegistry.count)
	buffered   []cli.EventEntry
	meta       node.AgentMetaPatch
	lastActive time.Time
	startedAt  time.Time
	closed     bool
}

// tailerRegistry struct + tailerKey + 11 methods (newTailerRegistry /
// startCentralPoller / pollLoop / snapshotTailers / ensureTailer / attach
// / detach / detachClient / closeTask / Shutdown / stopCentralPoller)
// moved to agent_tailer_registry.go (Phase 4c-prep, 2026-05-28).
//
// jsonlPathUnderAllowedRoot + resolveExistingAncestor moved to
// agent_tailer_pathcheck.go (Phase 4c-prep, 2026-05-28).

// MetaSnapshot returns a copy of the tailer's meta without mutating state.
// Consumed by enrichSnapshot.
func (t *agentTailer) MetaSnapshot() node.AgentMetaPatch {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.meta
}

// pollOnce reads the next slice of transcript events. Returns false when the
// tailer has self-terminated (idle grace expired) or has already been closed.
// R246-PERF-7: invoked by the registry's central pollLoop rather than a
// per-tailer goroutine; the t.closed check at the head handles the rare race
// where finalize() runs between the snapshot and this call.
func (t *agentTailer) pollOnce() bool {
	events, err := t.reader.Tail()
	if err != nil {
		slog.Debug("agent_tailer: tail error", "key", t.key, "task", t.taskID, "err", err)
	}

	// Capture wall clock outside the lock so the vDSO call does not extend
	// the per-tailer critical section. R040034-PERF-8 (#1407): every µs the
	// lock is held serialises concurrent attach/detach against the
	// per-tailer broadcast.
	now := time.Now()

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	if len(events) > 0 {
		t.lastActive = now
		// Buffer for late subscribers (bounded — 500 events). Past the cap
		// we drop oldest so memory stays bounded without silently forgetting
		// everything (keeps the tail).
		for _, e := range events {
			t.buffered = append(t.buffered, e)
			t.updateMetaFromEventLocked(e, now)
		}
		if over := len(t.buffered) - 500; over > 0 {
			// R228-PERF-13: drop the oldest `over` events while reusing the
			// existing backing array — copy the retained tail in-place to
			// the front, then truncate. R233-PERF-7: previously this copied
			// into a fresh backing array via `append(nil, t.buffered[over:]...)`,
			// allocating ~150 KB (500 × ~300B EventEntry) per overflow tick.
			// In-place copy reuses the original cap=500-headroom slice, so
			// steady-state runs at zero alloc once the slice has warmed up.
			//
			// Zero out the now-unreachable suffix BEFORE truncating so the
			// EventEntry pointers (Images / ToolCall / Message bytes) become
			// reachable for GC immediately rather than waiting for the next
			// overflow to overwrite them. Without this, dropped events
			// pin their referenced data through the next 500 events.
			n := copy(t.buffered, t.buffered[over:])
			for i := n; i < len(t.buffered); i++ {
				t.buffered[i] = cli.EventEntry{}
			}
			t.buffered = t.buffered[:n]
		}
	}
	t.mu.Unlock()

	// R040034-PERF-8 (#1407): take a SECOND brief lock for the subs+meta
	// snapshot + idle/refCount read rather than holding through the
	// buffer-mutation block. Splitting the window lets concurrent
	// attach/detach (which also need t.mu) interleave between the two,
	// reducing worst-case lock-hold latency on busy tailers. The
	// subs/meta+idle snapshot is independent of the buffer mutation: it
	// captures whoever is currently attached at this poll boundary, which
	// is what we want for the upcoming fan-out and for the
	// idle && refCount==0 close decision below.
	//
	// IMPORTANT: idle and refCount must be read in the SAME critical
	// section as the t.subs snapshot, otherwise an attach landing between
	// the two locks could leave us seeing refCount==0 alongside a
	// just-populated subs slice — wrongly closing a tailer that has a
	// fresh subscriber. The single second-lock window keeps these reads
	// consistent.
	//
	// Snapshot subs only when there are events to fan out — idle ticks
	// otherwise paid an O(N) map copy per poll for nothing. Also skip when
	// there are events but nobody is subscribed (silent buffer-only mode):
	// the events are already retained in t.buffered for late subscribers,
	// and allocating an empty []*wsClient just to range nothing is waste.
	//
	// R245-PERF-15 (#865): when subs IS populated, the slice came from a
	// sync.Pool so the per-tick allocation is a single slot lookup instead
	// of make([]*wsClient, 0, N). The pool's Put zero-clears the pointers
	// before returning the slice so wsClient closures cannot keep clients
	// alive past their unsubscribe. Pool reuse is the difference between
	// 5/s × 50 tailers × 1 alloc/tick (≈250/s, GC-visible) and ≈0/s for the
	// steady-state case where the same tailer keeps getting events from
	// the same set of subscribers.
	var subs []*wsClient
	var subsHandle tailerSubsHandle
	var meta node.AgentMetaPatch
	t.mu.Lock()
	if len(events) > 0 && len(t.subs) > 0 {
		subs, subsHandle = acquireTailerSubsSlice(len(t.subs))
		for c := range t.subs {
			subs = append(subs, c)
		}
		meta = t.meta
	}
	idle := now.Sub(t.lastActive) > agentTailerIdleGrace
	refCount := t.refCount.Load()
	t.mu.Unlock()
	defer releaseTailerSubsSlice(subs, subsHandle)

	// Broadcast new events. R231-PERF-5 / R232-PERF-2 / R233-PERF-* family:
	// when more than one subscriber is attached, marshal each frame once
	// and fan it out via SendRaw rather than letting every subscriber pay
	// the json.Marshal reflect cost. The single-subscriber path keeps the
	// SendJSON shortcut to avoid the marshalPooled buffer dance for the
	// common "one dashboard tab" case.
	for i := range events {
		e := events[i]
		if len(subs) == 1 {
			subs[0].SendJSON(node.ServerMsg{
				Type:   "agent_event",
				Key:    t.key,
				TaskID: t.taskID,
				Event:  &e,
			})
			continue
		}
		data, err := marshalPooled(node.ServerMsg{
			Type:   "agent_event",
			Key:    t.key,
			TaskID: t.taskID,
			Event:  &e,
		})
		if err != nil {
			// Marshal of a non-cyclic struct cannot fail in practice;
			// fall back to per-subscriber SendJSON so a future schema
			// regression still emits something instead of silently
			// dropping the frame.
			for _, c := range subs {
				c.SendJSON(node.ServerMsg{
					Type:   "agent_event",
					Key:    t.key,
					TaskID: t.taskID,
					Event:  &e,
				})
			}
			continue
		}
		for _, c := range subs {
			c.SendRaw(data)
		}
	}
	if len(events) > 0 && len(subs) > 0 {
		m := meta
		if len(subs) == 1 {
			subs[0].SendJSON(node.ServerMsg{
				Type:      "agent_meta",
				Key:       t.key,
				TaskID:    t.taskID,
				AgentMeta: &m,
			})
		} else {
			data, err := marshalPooled(node.ServerMsg{
				Type:      "agent_meta",
				Key:       t.key,
				TaskID:    t.taskID,
				AgentMeta: &m,
			})
			if err != nil {
				for _, c := range subs {
					c.SendJSON(node.ServerMsg{
						Type:      "agent_meta",
						Key:       t.key,
						TaskID:    t.taskID,
						AgentMeta: &m,
					})
				}
			} else {
				for _, c := range subs {
					c.SendRaw(data)
				}
			}
		}
	}

	// Idle reap — only when truly silent (no subscribers + no recent growth).
	// idle, refCount, and the subs snapshot above were all read inside the
	// same t.mu critical section, so this is not a TOCTOU: the three
	// values describe a consistent point in time and t.lastActive cannot
	// have advanced after the unlock without a new event arriving in a
	// subsequent pollOnce. R228-GO-P3-2.
	if idle && refCount == 0 {
		t.reg.closeTask(t.key, t.taskID, "")
		return false
	}
	return true
}

// updateMetaFromEventLocked refreshes meta counters from a single event.
// `now` is the wall clock captured by the caller — currently outside the
// lock per R040034-PERF-8 (#1407) so the vDSO call does not extend the
// per-tailer critical section — but all events in a single pollOnce share
// one reading so DurationMS is consistent and per-event time.Since vDSO
// calls are avoided. The caller still holds t.mu when invoking this
// helper so the meta writes themselves remain mutually exclusive with
// MetaSnapshot readers. R228-PERF-4.
func (t *agentTailer) updateMetaFromEventLocked(e cli.EventEntry, now time.Time) {
	switch e.Type {
	case "tool_use":
		t.meta.ToolUses++
		if e.Tool != "" {
			t.meta.LastTool = e.Tool
		}
		if e.Summary != "" {
			t.meta.LastDetail = e.Summary
		}
	case "tool_result":
		// Leaves ToolUses alone (assistant tool_use already counted), but
		// update LastDetail so the banner stat line reflects the latest
		// step. Mirrors the "counts = tool_use count, detail = most recent
		// surface" contract the parent stream uses.
		if e.Summary != "" {
			t.meta.LastDetail = e.Summary
		}
	case "thinking":
		// thinking is not counted as a tool use but does advance the "what
		// is this agent doing right now" line.
		t.meta.LastTool = "thinking"
	}
	if !t.startedAt.IsZero() {
		t.meta.DurationMS = now.Sub(t.startedAt).Milliseconds()
	}
}

// finalize stops the tailer, fires agent_done to all subscribers, and nudges
// a final agent_meta so the banner row's final "N calls · 2.1s" stays
// accurate even after the user's view has unsubscribed.
func (t *agentTailer) finalize(status string) {
	t.doneOnce.Do(func() {
		close(t.stopCh)
		t.mu.Lock()
		t.closed = true
		subs := make([]*wsClient, 0, len(t.subs))
		for c := range t.subs {
			subs = append(subs, c)
		}
		meta := t.meta
		t.subs = nil
		t.refCount.Store(0)
		t.mu.Unlock()

		// R20260605B-CORR-3 (#1807): release the persistent transcript fd
		// eagerly instead of waiting on the *os.File GC finalizer. finalize
		// is the single teardown funnel for every path (closeTask / idle
		// reap / Shutdown), and t.closed=true above plus the byTask removal
		// done by the caller stop the central poller from re-Tailing this
		// tailer, so the reader will not be reopened. Close() is idempotent
		// (R233-PERF-4), so a stray late poll that reopens is harmless.
		if t.reader != nil {
			_ = t.reader.Close()
		}

		if status == "" {
			status = "completed"
		}
		m := meta
		metaMsg := node.ServerMsg{
			Type:      "agent_meta",
			Key:       t.key,
			TaskID:    t.taskID,
			AgentMeta: &m,
		}
		doneMsg := node.ServerMsg{
			Type:   "agent_done",
			Key:    t.key,
			TaskID: t.taskID,
			Status: status,
		}
		// Mirror pollOnce: for a multi-tab fan-out marshal each terminal
		// frame once and SendRaw, rather than paying the json.Marshal
		// reflect cost per subscriber. The single-subscriber case keeps the
		// SendJSON shortcut; a marshal error falls back to per-sub SendJSON.
		if len(subs) == 1 {
			subs[0].SendJSON(metaMsg)
			subs[0].SendJSON(doneMsg)
			return
		}
		metaData, metaErr := marshalPooled(metaMsg)
		doneData, doneErr := marshalPooled(doneMsg)
		for _, c := range subs {
			if metaErr != nil {
				c.SendJSON(metaMsg)
			} else {
				c.SendRaw(metaData)
			}
			if doneErr != nil {
				c.SendJSON(doneMsg)
			} else {
				c.SendRaw(doneData)
			}
		}
	})
}
