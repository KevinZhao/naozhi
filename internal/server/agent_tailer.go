package server

import (
	"log/slog"
	"path/filepath"
	"strings"
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

// tailerSubsPool reuses []*wsClient slices across pollOnce ticks so the
// 200ms event-fanout path stays alloc-free in steady state. R245-PERF-15
// (#865): each pollOnce previously did make([]*wsClient, 0, len(t.subs))
// per tick when there were events to fan out — at 50 active tailers ×
// 5 ticks/s that's 250 GC-visible allocs/s for slices that never escape.
// The pool's New supplies a 4-cap default (matches the typical "1-2
// dashboard tabs subscribed" steady state); larger fan-outs grow the
// underlying slice as usual and the grown slice is then returned to the
// pool, so subsequent ticks with similar subscriber counts skip the
// growth too.
//
// We pool *[]*wsClient (pointer-to-slice) per the sync.Pool best-practice
// note in the standard library — putting the slice header by value means
// every Get/Put cycle allocates a new interface{} header for the slice
// metadata, which would defeat half the pool's purpose. The pointer
// indirection lets the same allocation round-trip end-to-end.
//
// releaseTailerSubsSlice zero-clears the slice so wsClient pointers
// cannot keep clients alive past their unsubscribe — without this, a
// busy tailer's pool would pin one wsClient per parked slot for the
// lifetime of the pool entry.
var tailerSubsPool = sync.Pool{
	New: func() any {
		s := make([]*wsClient, 0, 4)
		return &s
	},
}

// tailerSubsHandle wraps the pool entry pointer so callers can return
// the *exact same* pointer they pulled from Get(). Without this, the
// caller would have to remember the original *[]*wsClient through the
// pollOnce control flow — and writing tailerSubsPool.Put(&local) would
// force `local` to escape to the heap (one alloc per tick), defeating
// the pool's purpose.
type tailerSubsHandle struct {
	sp *[]*wsClient
}

// acquireTailerSubsSlice returns a reusable []*wsClient with len==0 and
// cap >= hint plus the handle the caller must hand back to release. The
// caller appends as if it were a fresh slice; only releaseTailerSubsSlice
// may return it to the pool.
func acquireTailerSubsSlice(hint int) ([]*wsClient, tailerSubsHandle) {
	sp := tailerSubsPool.Get().(*[]*wsClient)
	s := (*sp)[:0]
	if cap(s) < hint {
		s = make([]*wsClient, 0, hint)
	}
	*sp = s
	return s, tailerSubsHandle{sp: sp}
}

// releaseTailerSubsSlice clears the slice's backing pointers (so dropped
// wsClients become GC-eligible immediately) and returns it to the pool
// via the handle the caller received from acquireTailerSubsSlice. The
// final s value supersedes whatever sat in the pool (caller may have
// grown the slice via append). Nil-handle-safe so the caller can defer
// the release unconditionally even on the no-subs branch.
func releaseTailerSubsSlice(s []*wsClient, h tailerSubsHandle) {
	if h.sp == nil {
		return
	}
	for i := range s {
		s[i] = nil
	}
	*h.sp = s[:0]
	tailerSubsPool.Put(h.sp)
}

// tailerBufferedPool reuses []cli.EventEntry buffers used by attach()
// to copy the in-memory ring under lock and replay events to a new
// subscriber outside the lock. Without the pool, every agent_subscribe
// path allocated a fresh buffer of up to 500 EventEntry values
// (~140 KB) inside t.mu — the lock window is short, but the allocation
// itself is GC-visible and was the #1 attach-path alloc per
// R249-PERF-4 (#926) profiling. Reuse pattern matches tailerSubsPool:
// pointer-to-slice in the pool so the slice metadata round-trips on
// the same heap object, and a handle wrapper so the same pointer comes
// back through Put.
//
// Default cap is 16 (the typical attach replays a handful of events
// for a fresh tab joining mid-run); larger replays grow the slice via
// append() inside attach() and the grown slice returns to the pool so
// subsequent attaches at similar sizes skip the growth.
var tailerBufferedPool = sync.Pool{
	New: func() any {
		s := make([]cli.EventEntry, 0, 16)
		return &s
	},
}

// tailerBufferedHandle wraps the pool entry pointer so attach() can
// hand back the *exact* pointer it pulled. Same rationale as
// tailerSubsHandle (taking &local would force the local to escape to
// the heap on every attach call).
type tailerBufferedHandle struct {
	sp *[]cli.EventEntry
}

// acquireTailerBufferedSlice returns a reusable []cli.EventEntry with
// len==0 and cap >= hint plus the handle the caller must hand back.
// R249-PERF-4 (#926).
func acquireTailerBufferedSlice(hint int) ([]cli.EventEntry, tailerBufferedHandle) {
	sp := tailerBufferedPool.Get().(*[]cli.EventEntry)
	s := (*sp)[:0]
	if cap(s) < hint {
		s = make([]cli.EventEntry, 0, hint)
	}
	*sp = s
	return s, tailerBufferedHandle{sp: sp}
}

// releaseTailerBufferedSlice zero-clears each EventEntry (so its
// embedded pointers — Images, ToolCall, Message bytes — become
// GC-eligible immediately rather than pinning whatever the previous
// attach handed us) and returns the slice to the pool. Nil-handle-safe.
func releaseTailerBufferedSlice(s []cli.EventEntry, h tailerBufferedHandle) {
	if h.sp == nil {
		return
	}
	for i := range s {
		s[i] = cli.EventEntry{}
	}
	*h.sp = s[:0]
	tailerBufferedPool.Put(h.sp)
}

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

// jsonlPathUnderAllowedRoot returns true when jsonlPath is anchored under
// allowedRoot. Pure prefix check is unsafe ("/var/foo" prefix-matches
// "/var/fooBar"), so anchor on the cleaned root + os.PathSeparator. This
// guard is defence-in-depth (ensureTailer's caller already operates on
// CLI-emitted paths under the workspace), not a TOCTOU-safe gate.
// R232-SEC-14.
//
// R260528-SEC-3: also EvalSymlinks both sides before the prefix check to
// align with the dashboard_cron_transcript handler's stricter pattern.
// macOS canonicalises /var → /private/var, and any host where allowedRoot
// contains a symlinked component (Docker bind-mounts, AMI-customised
// layouts) drifts under EvalSymlinks; without the symmetric resolve the
// prefix check would reject every legitimate path on those hosts.
// EvalSymlinks failures fall through to the original (cleaned) value
// rather than reject — a broken symlink in the resolved chain or a
// path-not-yet-materialised must not turn the lexical HasPrefix gate
// into a hard production deny.
func jsonlPathUnderAllowedRoot(jsonlPath, allowedRoot string) bool {
	abs := filepath.Clean(jsonlPath)
	if !filepath.IsAbs(abs) {
		return false
	}
	root := filepath.Clean(allowedRoot)
	// EvalSymlinks of root + abs to align with transcript handler. Failure
	// (path missing, broken chain, permission denied) keeps the original
	// cleaned value so a transient FS state never produces a false reject.
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
	}
	if resolvedAbs, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolvedAbs
	}
	if abs == root {
		return false
	}
	prefix := root + string(filepath.Separator)
	return strings.HasPrefix(abs, prefix)
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

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	now := time.Now()
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
	if len(events) > 0 && len(t.subs) > 0 {
		subs, subsHandle = acquireTailerSubsSlice(len(t.subs))
		for c := range t.subs {
			subs = append(subs, c)
		}
		meta = t.meta
	}
	defer releaseTailerSubsSlice(subs, subsHandle)
	idle := now.Sub(t.lastActive) > agentTailerIdleGrace
	refCount := t.refCount.Load()
	t.mu.Unlock()

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
// `now` is the wall clock captured by the caller under t.mu so all events
// in a single pollOnce share one DurationMS reading and avoid per-event
// time.Since vDSO calls. R228-PERF-4.
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
