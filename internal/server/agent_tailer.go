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

// tailerRegistry is per-Hub and owns all active agentTailers. Installed via
// Hub.InitAgentTailers() once the Hub is constructed (called from server.go).
type tailerRegistry struct {
	mu         sync.RWMutex
	byTask     map[tailerKey]*agentTailer
	count      atomic.Int32
	hub        *Hub
	clientSubs map[*wsClient]map[tailerKey]struct{} // reverse index for client teardown

	// R246-PERF-7: a single registry-level ticker drives every tailer's
	// pollOnce step rather than each tailer owning its own goroutine +
	// time.NewTicker. With agentTailerMax=50 the prior design cost 50
	// long-lived goroutines and 50 ticker channels; this collapses to one
	// of each. pollWG tracks the central ticker goroutine so Shutdown can
	// block until the final fan-out iteration has fully released every
	// t.reader before the Hub-level teardown drops the transcript file.
	pollStop     chan struct{}
	pollStopOnce sync.Once
	pollOnce     sync.Once
	pollWG       sync.WaitGroup
}

type tailerKey struct {
	key    string
	taskID string
}

// newTailerRegistry wires a registry onto a Hub.
func newTailerRegistry(hub *Hub) *tailerRegistry {
	return &tailerRegistry{
		byTask:     make(map[tailerKey]*agentTailer),
		hub:        hub,
		clientSubs: make(map[*wsClient]map[tailerKey]struct{}),
		pollStop:   make(chan struct{}),
	}
}

// startCentralPoller lazily launches the single registry-level pollLoop
// goroutine on first ensureTailer. Doing the launch on demand (rather than at
// newTailerRegistry time) keeps test fixtures that build registries without
// ever calling ensureTailer goroutine-leak-free, and matches the prior
// "goroutine spawned only after first tailer" footprint.
func (r *tailerRegistry) startCentralPoller() {
	r.pollOnce.Do(func() {
		r.pollWG.Add(1)
		go r.pollLoop()
	})
}

// pollLoop is the single timer goroutine that drives every active tailer's
// pollOnce step. R246-PERF-7: replaces the per-tailer t.run() goroutine that
// allocated one *time.Ticker + one goroutine per tailer (×agentTailerMax=50).
// Iteration is serial — pollOnce is a bounded operation (a few file Stat /
// Read calls + bounded send-fan-out under the tailer's own mu) and 50 of
// them per 200 ms tick comfortably fits in budget. A central goroutine also
// gives idle reaping a single deterministic point of execution.
func (r *tailerRegistry) pollLoop() {
	defer r.pollWG.Done()
	ticker := time.NewTicker(agentTailerPollInterval)
	defer ticker.Stop()
	// Reuse a single scratch slice across ticks so steady-state polling pays
	// zero allocation for the snapshot itself; the cap grows monotonically up
	// to agentTailerMax.
	scratch := make([]*agentTailer, 0, agentTailerMax)
	for {
		select {
		case <-r.pollStop:
			return
		case <-ticker.C:
			scratch = r.snapshotTailers(scratch[:0])
			for _, t := range scratch {
				if !t.pollOnce() {
					// pollOnce returned false (idle reap or already closed).
					// closeTask has already removed t from byTask so the next
					// snapshot will not see it. No further work here.
				}
			}
			// Drop references in the scratch slice between ticks so a tailer
			// removed by closeTask becomes GC-eligible immediately rather than
			// being pinned through r.pollWG's scratch capture.
			// R249-PERF-5: clear() compiles to a memclr that no-ops on an empty
			// slice, so the steady-state idle tick (no tailers registered) pays
			// zero work here instead of entering a manual range loop.
			clear(scratch)
		}
	}
}

// snapshotTailers copies the current set of live tailers into dst (truncated
// by the caller) under r.mu.RLock, returning the populated slice. Holds the
// read lock for O(N) over byTask only — pollOnce iteration runs lock-free
// outside.
func (r *tailerRegistry) snapshotTailers(dst []*agentTailer) []*agentTailer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.byTask {
		dst = append(dst, t)
	}
	return dst
}

// jsonlPathUnderAllowedRoot + resolveExistingAncestor moved to
// agent_tailer_pathcheck.go (Phase 4c-prep, 2026-05-28).

// ensureTailer is called by the Linker OnResolve callback or by an
// agent_subscribe message before the silent tailer has started. Idempotent:
// repeated calls for the same (key, taskID) return the existing tailer.
// Returns (nil, false) when the cap has been hit — caller must emit
// agent_subscribe_rejected.
func (r *tailerRegistry) ensureTailer(key, taskID, toolUseID, jsonlPath string) (*agentTailer, bool) {
	if jsonlPath == "" {
		return nil, false
	}
	// R232-SEC-14: when allowedRoot is configured, refuse jsonlPath outside
	// it. The Linker normally sources JSONLPath from CLI-emitted absolute
	// paths under the workspace root, but a malformed init/parallel-stream
	// event could in principle drive ensureTailer at an arbitrary FS
	// location and have the silent tailer Stat/Tail it. Empty allowedRoot
	// (legacy unrestricted deployments) keeps prior behaviour.
	if r != nil && r.hub != nil && r.hub.allowedRoot != "" {
		if !jsonlPathUnderAllowedRoot(jsonlPath, r.hub.allowedRoot) {
			slog.Warn("agent_tailer: jsonl path outside allowed_root rejected",
				"key", key, "task", taskID, "path", jsonlPath)
			return nil, false
		}
	}
	tk := tailerKey{key, taskID}

	r.mu.RLock()
	if t, ok := r.byTask[tk]; ok {
		r.mu.RUnlock()
		return t, true
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.byTask[tk]; ok {
		return t, true
	}
	if r.count.Load() >= agentTailerMax {
		return nil, false
	}
	t := &agentTailer{
		key:        key,
		taskID:     taskID,
		toolUseID:  toolUseID,
		reader:     cli.NewTranscriptReader(jsonlPath),
		reg:        r,
		hub:        r.hub,
		stopCh:     make(chan struct{}),
		subs:       make(map[*wsClient]struct{}),
		lastActive: time.Now(),
		startedAt:  time.Now(),
	}
	r.byTask[tk] = t
	r.count.Add(1)
	// R246-PERF-7: spin up the single central poller on first tailer rather
	// than spawning a goroutine per tailer. Idempotent via sync.Once.
	r.startCentralPoller()
	return t, true
}

// attach adds a client to the tailer and flushes buffered events to them.
// Called by agent_subscribe handler after ensureTailer returns a live tailer.
// Returns false when the tailer has already closed (stale subscribe).
func (r *tailerRegistry) attach(tk tailerKey, c *wsClient) bool {
	r.mu.Lock()
	t, ok := r.byTask[tk]
	if ok {
		subs, found := r.clientSubs[c]
		if !found {
			subs = make(map[tailerKey]struct{})
			r.clientSubs[c] = subs
		}
		subs[tk] = struct{}{}
	}
	r.mu.Unlock()
	if !ok {
		return false
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	if _, exists := t.subs[c]; !exists {
		t.subs[c] = struct{}{}
		t.refCount.Add(1)
	}
	// R249-PERF-4 (#926): pool the replay buffer instead of allocating
	// a fresh up-to-500-element []cli.EventEntry per attach. The pool
	// is keyed by len(t.buffered) so a tab joining a hot run with 500
	// buffered events grows the underlying slice once and subsequent
	// attaches at similar buffer depths reuse the grown slice. The
	// copy-out under lock is unchanged — we still snapshot the ring
	// here so the replay loop below runs lock-free; only the
	// destination slice is now reused across calls.
	buffered, bufferedHandle := acquireTailerBufferedSlice(len(t.buffered))
	buffered = append(buffered, t.buffered...)
	meta := t.meta
	t.mu.Unlock()

	// Replay buffered events to the new subscriber outside the lock so a
	// slow client cannot stall other subscribers. Defer the pool
	// release until after the replay so the SendJSON loop can borrow
	// the slice without a copy; releaseTailerBufferedSlice zero-clears
	// each EventEntry before returning the slice so the pool does not
	// pin Images / ToolCall / Message pointers across calls.
	defer releaseTailerBufferedSlice(buffered, bufferedHandle)
	for i := range buffered {
		e := buffered[i]
		c.SendJSON(node.ServerMsg{
			Type:   "agent_event",
			Key:    t.key,
			TaskID: t.taskID,
			Event:  &e,
		})
	}
	if meta.ToolUses > 0 || meta.DurationMS > 0 || meta.LastTool != "" {
		m := meta
		c.SendJSON(node.ServerMsg{
			Type:      "agent_meta",
			Key:       t.key,
			TaskID:    t.taskID,
			AgentMeta: &m,
		})
	}
	return true
}

// detach removes a client from a specific tailer. A refCount drop to zero
// does NOT stop the tailer — it keeps running silent so parent-stream
// task_done can still fire agent_done to any rejoining subscribers.
func (r *tailerRegistry) detach(tk tailerKey, c *wsClient) {
	r.mu.Lock()
	t := r.byTask[tk]
	if subs, ok := r.clientSubs[c]; ok {
		delete(subs, tk)
		if len(subs) == 0 {
			delete(r.clientSubs, c)
		}
	}
	r.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	if _, ok := t.subs[c]; ok {
		delete(t.subs, c)
		t.refCount.Add(-1)
	}
	t.mu.Unlock()
}

// detachClient removes `c` from every tailer it subscribed to. Called from
// wsClient teardown so abrupt disconnects don't leak subscriptions.
func (r *tailerRegistry) detachClient(c *wsClient) {
	r.mu.Lock()
	subs, ok := r.clientSubs[c]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.clientSubs, c)
	// R249-PERF-3 (#925): cold-path detach but no reason to spend two
	// allocations. The previous shape built keys[], then walked it to
	// build targets[] — keys was dead the moment targets finished. Walk
	// subs directly into targets[] and skip the intermediate slice.
	targets := make([]*agentTailer, 0, len(subs))
	for k := range subs {
		if t, ok := r.byTask[k]; ok {
			targets = append(targets, t)
		}
	}
	r.mu.Unlock()
	for _, t := range targets {
		t.mu.Lock()
		if _, ok := t.subs[c]; ok {
			delete(t.subs, c)
			t.refCount.Add(-1)
		}
		t.mu.Unlock()
	}
}

// closeTask stops the tailer for (key, taskID) and fires agent_done to any
// remaining subscribers. Called by the Linker's task_done forwarder or by
// the idle sweep path. Status: "completed"|"error".
func (r *tailerRegistry) closeTask(key, taskID, status string) {
	tk := tailerKey{key, taskID}
	r.mu.Lock()
	t, ok := r.byTask[tk]
	if ok {
		delete(r.byTask, tk)
		r.count.Add(-1)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	t.finalize(status)
}

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

		if status == "" {
			status = "completed"
		}
		m := meta
		for _, c := range subs {
			c.SendJSON(node.ServerMsg{
				Type:      "agent_meta",
				Key:       t.key,
				TaskID:    t.taskID,
				AgentMeta: &m,
			})
			c.SendJSON(node.ServerMsg{
				Type:   "agent_done",
				Key:    t.key,
				TaskID: t.taskID,
				Status: status,
			})
		}
	})
}

// Shutdown stops every tailer the registry owns. Called by Hub.Shutdown.
// Blocks until the central pollLoop goroutine has returned so the surrounding
// Hub teardown can drop the underlying TranscriptReader without racing the
// final pollOnce iteration. R246-PERF-7: previously waited on runWG (one
// goroutine per tailer); now waits on pollWG (one goroutine total).
func (r *tailerRegistry) Shutdown() {
	r.mu.Lock()
	tailers := make([]*agentTailer, 0, len(r.byTask))
	for _, t := range r.byTask {
		tailers = append(tailers, t)
	}
	// R247-PERF-25: clear() reuses the existing map's bucket array instead
	// of paying the runtime.makemap allocation each Shutdown. The registry
	// is rebuilt from empty after Shutdown() returns (the next ensureTailer
	// re-populates), so reusing the underlying bucket slab is safe — no
	// stale pointer is observable through the cleared map.
	clear(r.byTask)
	clear(r.clientSubs)
	r.count.Store(0)
	r.mu.Unlock()
	for _, t := range tailers {
		t.finalize("shutdown")
	}
	// Stop the central poller exactly once. close on a never-started channel
	// is fine, but startCentralPoller may have launched the loop at any point;
	// we use a sync.Once guard to make Shutdown idempotent for tests that
	// call it more than once.
	r.stopCentralPoller()
	r.pollWG.Wait()
}

// stopCentralPoller signals the central pollLoop goroutine to exit. Safe to
// call multiple times — the underlying close is sync.Once-guarded via
// pollStopOnce. Tailer registries that never had ensureTailer called still
// observe this no-op cleanly because pollLoop was never started (pollWG.Wait
// returns immediately).
func (r *tailerRegistry) stopCentralPoller() {
	r.pollStopOnce.Do(func() {
		close(r.pollStop)
	})
}
