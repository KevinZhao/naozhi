// tailerRegistry 容器侧（生命周期管理、ensureTailer、attach/detach、Shutdown）；
// agentTailer 本体见 agent_tailer.go。
package server

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// tailerRegistry is per-Hub and owns all active agentTailers. Installed via
// Hub.InitAgentTailers() once the Hub is constructed (called from server.go).
type tailerRegistry struct {
	mu         sync.RWMutex
	byTask     map[tailerKey]*agentTailer
	count      atomic.Int32
	hub        *Hub
	clientSubs map[*wsClient]map[tailerKey]struct{} // reverse index for client teardown

	// One registry-level ticker drives every tailer's pollOnce. pollWG lets
	// Shutdown block until the final iteration has released every t.reader.
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

// startCentralPoller lazily launches the single pollLoop goroutine on first
// ensureTailer, so registries that never tail stay goroutine-free.
func (r *tailerRegistry) startCentralPoller() {
	r.pollOnce.Do(func() {
		r.pollWG.Add(1)
		go r.pollLoop()
	})
}

// pollLoop is the single timer goroutine that drives every active tailer's
// pollOnce step. Iteration is serial — pollOnce is bounded and 50 per
// 200 ms tick fits in budget; it also gives idle reaping one execution point.
func (r *tailerRegistry) pollLoop() {
	defer r.pollWG.Done()
	ticker := time.NewTicker(agentTailerPollInterval)
	defer ticker.Stop()
	// Scratch slice reused across ticks so the snapshot is alloc-free.
	scratch := make([]*agentTailer, 0, agentTailerMax)
	for {
		select {
		case <-r.pollStop:
			return
		case <-ticker.C:
			scratch = r.snapshotTailers(scratch[:0])
			for _, t := range scratch {
				if !t.pollOnce() {
					// Idle reap or already closed; closeTask already removed t from byTask.
				}
			}
			// Drop references between ticks so a closed tailer is GC-eligible
			// immediately instead of pinned by scratch.
			clear(scratch)
		}
	}
}

// snapshotTailers copies the current set of live tailers into dst (truncated
// by the caller) under r.mu.RLock; pollOnce iteration then runs lock-free.
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
	// When allowedRoot is configured, refuse jsonlPath outside it so a
	// malformed CLI event cannot make the tailer Stat/Tail an arbitrary file.
	// Empty allowedRoot means unrestricted.
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
	// attach/detachClient race guard: writePump may have already run the
	// one-and-only detachClient; re-inserting the dead client would pin it in
	// t.subs forever (refCount stuck at 1). c.done is observed under t.mu, so
	// if it is closed we bail and roll back the clientSubs entry (#2259).
	select {
	case <-c.done:
		t.mu.Unlock()
		r.mu.Lock()
		if subs, found := r.clientSubs[c]; found {
			delete(subs, tk)
			if len(subs) == 0 {
				delete(r.clientSubs, c)
			}
		}
		r.mu.Unlock()
		return false
	default:
	}
	if _, exists := t.subs[c]; !exists {
		t.subs[c] = struct{}{}
		t.refCount.Add(1)
	}
	// Snapshot the ring into a pooled buffer so the replay below runs
	// lock-free (#926). Empty buffer skips the pool entirely; meta is still
	// snapshotted so the late-meta nudge stays correct.
	var buffered []clievent.EventEntry
	var bufferedHandle tailerBufferedHandle
	if len(t.buffered) > 0 {
		buffered, bufferedHandle = acquireTailerBufferedSlice(len(t.buffered))
		buffered = append(buffered, t.buffered...)
	}
	meta := t.meta
	t.mu.Unlock()

	// Replay outside the lock so a slow client cannot stall other
	// subscribers; release (nil-handle-safe) deferred until after replay.
	defer releaseTailerBufferedSlice(buffered, bufferedHandle)
	for i := range buffered {
		e := buffered[i]
		c.SendJSON(wsproto.NewAgentEvent(wsproto.AgentEvent{

			Key:    t.key,
			TaskID: t.taskID,
			Event:  &e,
		}))
	}
	if meta.ToolUses > 0 || meta.DurationMS > 0 || meta.LastTool != "" {
		m := meta
		c.SendJSON(wsproto.NewAgentMeta(wsproto.AgentMeta{

			Key:       t.key,
			TaskID:    t.taskID,
			AgentMeta: &m,
		}))
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
// Blocks until the central pollLoop goroutine has returned so Hub teardown
// cannot race the final pollOnce iteration.
func (r *tailerRegistry) Shutdown() {
	r.mu.Lock()
	tailers := make([]*agentTailer, 0, len(r.byTask))
	for _, t := range r.byTask {
		tailers = append(tailers, t)
	}
	clear(r.byTask)
	clear(r.clientSubs)
	r.count.Store(0)
	r.mu.Unlock()
	for _, t := range tailers {
		t.finalize("shutdown")
	}
	r.stopCentralPoller()
	r.pollWG.Wait()
}

// stopCentralPoller signals the central pollLoop goroutine to exit. Safe to
// call multiple times and when pollLoop was never started.
func (r *tailerRegistry) stopCentralPoller() {
	r.pollStopOnce.Do(func() {
		close(r.pollStop)
	})
}
