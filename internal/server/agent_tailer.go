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
	// runWG tracks every spawned t.run() goroutine so Shutdown can
	// wait for the final pollOnce iteration to release its reference
	// to t.reader before Hub teardown continues. Without this the
	// goroutine can race with the surrounding Hub.Shutdown drain when
	// the underlying transcript file is being torn down (-race detector
	// flags the access).
	runWG sync.WaitGroup
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
	}
}

// jsonlPathUnderAllowedRoot returns true when jsonlPath is anchored under
// allowedRoot. Pure prefix check is unsafe ("/var/foo" prefix-matches
// "/var/fooBar"), so anchor on the cleaned root + os.PathSeparator. Symlinks
// are not resolved here: ensureTailer's caller (the Linker OnResolve handler)
// already operates on CLI-emitted paths inside the workspace; this guard is
// defence-in-depth, not a TOCTOU-safe gate. R232-SEC-14.
func jsonlPathUnderAllowedRoot(jsonlPath, allowedRoot string) bool {
	abs := filepath.Clean(jsonlPath)
	if !filepath.IsAbs(abs) {
		return false
	}
	root := filepath.Clean(allowedRoot)
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
		// R235-PERF-4: pre-size the late-subscriber buffer to its 500-entry
		// cap. Without this, the implicit nil slice grows via the runtime's
		// doubling rule (1→2→4→…→512 = ~9 reallocations) before the first
		// overflow trim. Each EventEntry is ~300 bytes so the doubling path
		// allocates ~150 KB of orphan backing arrays per fresh tailer.
		buffered: make([]cli.EventEntry, 0, 500),
	}
	r.byTask[tk] = t
	r.count.Add(1)
	r.runWG.Add(1)
	go func() {
		defer r.runWG.Done()
		t.run()
	}()
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
	buffered := make([]cli.EventEntry, len(t.buffered))
	copy(buffered, t.buffered)
	meta := t.meta
	t.mu.Unlock()

	// Replay buffered events to the new subscriber outside the lock so a
	// slow client cannot stall other subscribers.
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
	keys := make([]tailerKey, 0, len(subs))
	for k := range subs {
		keys = append(keys, k)
	}
	targets := make([]*agentTailer, 0, len(keys))
	for _, k := range keys {
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

func (t *agentTailer) run() {
	ticker := time.NewTicker(agentTailerPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			if !t.pollOnce() {
				return
			}
		}
	}
}

// pollOnce reads the next slice of transcript events. Returns false when the
// tailer has self-terminated (idle grace expired).
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
	var subs []*wsClient
	var meta node.AgentMetaPatch
	if len(events) > 0 && len(t.subs) > 0 {
		subs = make([]*wsClient, 0, len(t.subs))
		for c := range t.subs {
			subs = append(subs, c)
		}
		meta = t.meta
	}
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
// Blocks until every t.run() goroutine has returned so the surrounding
// Hub teardown can drop the underlying TranscriptReader without racing
// the final pollOnce iteration.
func (r *tailerRegistry) Shutdown() {
	r.mu.Lock()
	tailers := make([]*agentTailer, 0, len(r.byTask))
	for _, t := range r.byTask {
		tailers = append(tailers, t)
	}
	r.byTask = make(map[tailerKey]*agentTailer)
	r.clientSubs = make(map[*wsClient]map[tailerKey]struct{})
	r.count.Store(0)
	r.mu.Unlock()
	for _, t := range tailers {
		t.finalize("shutdown")
	}
	r.runWG.Wait()
}
