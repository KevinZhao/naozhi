// Phase 4c-prep / R-tailer-registry-extract (2026-05-28):
// tailerRegistry struct + tailerKey + 11 methods 抽到独立文件。
// 纯物理切分、零行为变化。
//
// agent_tailer.go 现在仅保留 agentTailer struct + 4 个 method
// (MetaSnapshot / pollOnce / updateMetaFromEventLocked / finalize)；
// 容器侧（生命周期管理、ensureTailer、attach/detach、Shutdown 等）
// 全部进入此文件。
package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

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
