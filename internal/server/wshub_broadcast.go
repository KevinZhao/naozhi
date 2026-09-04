// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     broadcast block (debounceMu / debounceTimer / debounceFirst /
//	            debounceClosed / debounceClosedFast / debounceFire) +
//	            subscriber block (clients) for SendRaw fanout
//	READS:      shared deps block (read-only after ctor) + send block
//	            (queue / droppedTotal for broadcast-aware enqueue)
package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// sessionsUpdateMsg is the pre-marshaled sessions_update frame, derived from
// node.ServerMsg at package init so it cannot drift from the wire schema while
// keeping the broadcast hot path zero-alloc (#869).
var sessionsUpdateMsg = marshalSessionsUpdate()

func marshalSessionsUpdate() []byte {
	data, err := json.Marshal(node.ServerMsg{Type: "sessions_update"})
	if err != nil {
		panic("server: marshal sessions_update frame: " + err.Error())
	}
	return data
}

// broadcastSnapPoolMaxCap: snapshots grown past the connection ceiling are not
// returned to broadcastClientSnapPool, so a spike cannot pin an oversized array.
const broadcastSnapPoolMaxCap = maxWSConns

// subFilterChunk bounds how many candidates fanOutToSubscribers filters per
// h.mu.RLock acquisition, so register / unregister / markAuthenticated (write
// lock) can interleave instead of waiting behind a whole-fleet scan (#1925).
const subFilterChunk = 64

// broadcastClientSnapPool reuses []*wsClient backing arrays across broadcasts.
var broadcastClientSnapPool = sync.Pool{
	New: func() any {
		s := make([]*wsClient, 0, 32)
		return &s
	},
}

// releaseBroadcastSnap returns a fan-out snapshot to broadcastClientSnapPool.
// Slots are nil'd so disconnected clients can be GC'd before reuse; oversized
// arrays are replaced with a fresh small slice instead of being pooled.
func releaseBroadcastSnap(snapPtr *[]*wsClient, snap []*wsClient) {
	for i := range snap {
		snap[i] = nil
	}
	if cap(snap) <= broadcastSnapPoolMaxCap {
		*snapPtr = snap[:0]
	} else {
		*snapPtr = make([]*wsClient, 0, 32)
	}
	broadcastClientSnapPool.Put(snapPtr)
}

// broadcastToAuthenticated sends raw data to all authenticated WebSocket clients.
// The recipient snapshot is taken under authMu and released before the per-client
// SendRaw loop so register / unregister never serialise behind a broadcast.
func (h *Hub) broadcastToAuthenticated(data []byte) {
	snapPtr, snap := h.snapshotAuthenticated()
	for _, c := range snap {
		c.SendRaw(data)
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// snapshotAuthenticated returns a pooled snapshot of the clients that should
// receive an "all authenticated clients" broadcast. The caller MUST return the
// snapshot to the pool via releaseBroadcastSnap once the fan-out completes.
// The authClients mirror is read under its own authMu, not the Hub-wide h.mu
// (#1621); it is nil only for hand-rolled test hubs that bypass NewHub, which
// fall back to walking h.clients. authClients is fixed at NewHub, so the nil
// check is lock-free.
func (h *Hub) snapshotAuthenticated() (*[]*wsClient, []*wsClient) {
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]

	if h.authClients != nil {
		// Copy the slice mirror instead of ranging the map: one sequential
		// memmove under authMu.RLock (#2310).
		h.authMu.RLock()
		if n := len(h.authClientsSlice); n > 0 {
			if cap(snap) < n {
				snap = make([]*wsClient, n)
			} else {
				snap = snap[:n]
			}
			copy(snap, h.authClientsSlice)
		}
		h.authMu.RUnlock()
	} else {
		// Legacy fallback for hand-rolled hubs that do not initialise
		// authClients. Production hubs always go through NewHub.
		h.mu.RLock()
		for c := range h.clients {
			if c.authenticated.Load() {
				snap = append(snap, c)
			}
		}
		h.mu.RUnlock()
	}
	return snapPtr, snap
}

// marshalBroadcastAuth marshals v and fans it out to every authenticated client.
// The snapshot is taken first and doubles as the empty check (one authMu
// acquisition), so a hub with no recipients skips marshalPooled entirely
// (#2141). A marshal failure drops the frame: the WS payload structs are
// fixed-shape and cannot fail in practice, and dropping beats panicking the
// producer goroutine.
func (h *Hub) marshalBroadcastAuth(v any) {
	snapPtr, snap := h.snapshotAuthenticated()
	if len(snap) == 0 {
		releaseBroadcastSnap(snapPtr, snap)
		return
	}
	data, err := marshalPooled(v)
	if err != nil {
		releaseBroadcastSnap(snapPtr, snap)
		return
	}
	for _, c := range snap {
		c.SendRaw(data)
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// broadcastState sends a session_state message to ALL authenticated clients.
// This mirrors BroadcastSessionReady: the "running" start is sent to everyone,
// so the final state must also reach everyone — otherwise clients not subscribed
// to this session would see a stale "running" dot in the sidebar forever.
func (h *Hub) broadcastState(key, state, reason string) {
	h.marshalBroadcastAuth(node.ServerMsg{Type: "session_state", Key: key, State: state, Reason: reason})
}

// BroadcastSessionReady sends a session_state "running" to ALL authenticated clients
// so they can auto-subscribe. Unlike broadcastState, this is not limited to already-
// subscribed clients — needed for new sessions where nobody is subscribed yet.
func (h *Hub) BroadcastSessionReady(key string) {
	h.marshalBroadcastAuth(node.ServerMsg{Type: "session_state", Key: key, State: "running"})
}

// BroadcastSessionsUpdate debounces notifications: resets a 50ms timer on each
// call; the actual broadcast fires only when no further calls arrive within the
// window. A 500ms hard cap on the total debounce window guarantees the update
// eventually fires even under sustained bursts, so clients never miss a refresh.
func (h *Hub) BroadcastSessionsUpdate() {
	const (
		debounceInterval = 50 * time.Millisecond
		maxDebounceDelay = 500 * time.Millisecond
	)
	// Lock-free fast path: once Shutdown has published the flag every call is a
	// no-op. The authoritative debounceClosed check below still runs under the
	// mutex for callers that arrive before the flag publishes (#723).
	if h.debounceClosedFast.Load() {
		return
	}
	// Capture wall clock outside the critical section so the vDSO call
	// does not extend the mutex window.
	now := time.Now()
	h.debounceMu.Lock()
	defer h.debounceMu.Unlock()
	// Shutdown already drained the debounce WG slot; any new scheduling here
	// would either leak (callback never waited for) or race clientWG.Wait.
	if h.debounceClosed {
		return
	}
	if h.debounceArmed {
		if now.Sub(h.debounceFirst) >= maxDebounceDelay {
			// Hard cap reached — let the pending timer fire without resetting.
			return
		}
		// Reset on a timer whose AfterFunc already fired (callback blocked on
		// debounceMu) would schedule a SECOND run without a matching
		// clientWG.Add, breaking Shutdown's Wait. Stop() == false means the
		// in-flight callback will do the broadcast; it clears debounceArmed so
		// the next call re-arms via the idle branch below.
		if h.debounceTimer.Stop() {
			h.debounceTimer.Reset(debounceInterval)
		}
		return
	}
	h.debounceFirst = now
	// Track the AfterFunc callback via clientWG so Shutdown can wait for
	// any late-firing broadcast to finish touching the clients map. The
	// callback still runs even after Stop() if it had already fired and
	// was scheduled, so the tracking guards against a post-Shutdown race.
	h.clientWG.Add(1)
	// Production hubs pre-allocate debounceTimer in NewHub (bound to
	// h.debounceFire) so the idle→armed transition is a Reset with no timer
	// allocation (#1624). Hand-rolled test hubs leave it nil and fall back to a
	// per-call AfterFunc whose closure keeps the closed-check for Shutdown races.
	if h.debounceTimer != nil {
		h.debounceArmed = true
		h.debounceTimer.Reset(debounceInterval)
		return
	}
	fire := func() {
		defer h.clientWG.Done()
		h.debounceMu.Lock()
		h.debounceArmed = false
		closed := h.debounceClosed
		h.debounceMu.Unlock()
		if closed {
			return
		}
		h.doBroadcastSessionsUpdate()
	}
	h.debounceArmed = true
	h.debounceTimer = time.AfterFunc(debounceInterval, fire)
}

func (h *Hub) doBroadcastSessionsUpdate() {
	data := sessionsUpdateMsg
	h.broadcastToAuthenticated(data)
}

// cronRunStartedMsg / cronRunEndedMsg are the cron-run-history WS payloads.
// Result text is not carried inline; clients fetch it via
// /api/cron/jobs/<id>/runs/<runID> when needed.
type cronRunStartedMsg struct {
	Type      string `json:"type"`
	JobID     string `json:"job_id"`
	RunID     string `json:"run_id"`
	StartedAt int64  `json:"started_at"`
	Trigger   string `json:"trigger,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Fresh     bool   `json:"fresh,omitempty"`
}

type cronRunEndedMsg struct {
	Type       string `json:"type"`
	JobID      string `json:"job_id"`
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
}

// BroadcastCronRunStarted emits cron_run_started to authenticated clients.
// Called from the cron scheduler's onRunStarted hook (set in dashboard.go).
func (h *Hub) BroadcastCronRunStarted(jobID, runID string, startedAt time.Time, trigger, sessionID string, fresh bool) {
	// jobID / runID come from cron.generateHexID; sanitizeHexIDForBroadcast
	// skips SanitizeForLog's allocating slow path when the hex shape holds.
	h.marshalBroadcastAuth(cronRunStartedMsg{
		Type:      "cron_run_started",
		JobID:     sanitizeHexIDForBroadcast(jobID, 64),
		RunID:     sanitizeHexIDForBroadcast(runID, 64),
		StartedAt: startedAt.UnixMilli(),
		Trigger:   sanitizeTriggerForBroadcast(trigger),
		SessionID: sanitizeSessionIDForBroadcast(sessionID),
		Fresh:     fresh,
	})
}

// BroadcastCronRunEnded emits cron_run_ended for every terminal state
// (succeeded / failed / skipped / timed_out / canceled). The dashboard
// uses State to decide colour and whether to refetch the list (counters
// updated). errorMsg is already path-redacted + sanitised by the cron
// package's recordResultP0 → SanitizeForLog pipeline.
func (h *Hub) BroadcastCronRunEnded(jobID, runID, state string, startedAt, endedAt time.Time, durationMS int64, sessionID, errClass, errMsg, trigger string) {
	// errClass/trigger are typed enums today; sanitising anyway shields a future
	// path that derives them from external config from payload injection.
	h.marshalBroadcastAuth(cronRunEndedMsg{
		Type:       "cron_run_ended",
		JobID:      sanitizeHexIDForBroadcast(jobID, 64),
		RunID:      sanitizeHexIDForBroadcast(runID, 64),
		State:      state,
		StartedAt:  startedAt.UnixMilli(),
		EndedAt:    endedAt.UnixMilli(),
		DurationMS: durationMS,
		SessionID:  sanitizeSessionIDForBroadcast(sessionID),
		ErrorClass: osutil.SanitizeForLog(errClass, 64),
		ErrorMsg:   errMsg,
		Trigger:    sanitizeTriggerForBroadcast(trigger),
	})
}

// broadcastSessionSystemEvent pushes a synthetic `system`-type event frame to
// every authenticated client currently subscribed to `key`. A remote session's
// EventLog lives on the remote node and cannot be appended locally, so remote
// send/interrupt failures are fanned out over the same WS `event` frame that
// streamed remote events use, scoped to that key's subscribers (#433).
//
// summary MUST be caller-sanitised (osutil.SanitizeForLog): it is broadcast
// verbatim to dashboards and would otherwise be an injection primitive.
func (h *Hub) broadcastSessionSystemEvent(key, summary string) {
	if key == "" || summary == "" {
		return
	}
	h.fanOutToSubscribers(key, func() any {
		ev := clievent.EventEntry{
			Time:    time.Now().UnixMilli(),
			Type:    "system",
			Summary: summary,
		}
		return node.ServerMsg{Type: "event", Key: key, Event: &ev}
	})
}

// DroppedMessages returns the total number of messages dropped across all
// clients since the process started (lock-free atomic load).
func (h *Hub) DroppedMessages() int64 {
	return h.droppedTotal.Load()
}

// LegacySendInvokes returns the total number of times sessionSend fell
// through to the deprecated sessionSendLegacy path. Production Hubs wire a
// real MessageQueue and never increment this; once every test fixture does
// too, sessionSendLegacy can be deleted (#710).
func (h *Hub) LegacySendInvokes() int64 {
	if h == nil {
		return 0
	}
	return h.legacySendInvokes.Load()
}

// daemonRunStartedMsg / daemonRunEndedMsg are the sysession WS payloads
// (docs/rfc/system-session.md §9.4). They deliberately carry NO ErrorMsg:
// daemon Runner errors can echo user-prompt fragments, and broadcasting that
// to every authenticated dashboard would be cross-tenant leakage. Server-side
// slog still carries the full error.
type daemonRunStartedMsg struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	RunID     string `json:"run_id"`
	Trigger   string `json:"trigger,omitempty"`
	StartedAt int64  `json:"started_at"`
}

type daemonRunEndedMsg struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
}

// BroadcastDaemonRunStarted emits daemon_run_started. name / runID / trigger
// are compiled-in enums today; sanitising at the broadcast boundary is
// defence-in-depth against a future config-derived caller.
func (h *Hub) BroadcastDaemonRunStarted(name, runID, trigger string, startedAt time.Time) {
	h.marshalBroadcastAuth(daemonRunStartedMsg{
		Type:      "daemon_run_started",
		Name:      osutil.SanitizeForLog(name, 64),
		RunID:     sanitizeHexIDForBroadcast(runID, 64),
		Trigger:   osutil.SanitizeForLog(trigger, 32),
		StartedAt: startedAt.UnixMilli(),
	})
}

// BroadcastDaemonRunEnded emits daemon_run_ended.  ErrorMsg is
// intentionally absent — see daemonRunEndedMsg above.
func (h *Hub) BroadcastDaemonRunEnded(name, runID, state, errClass, trigger string, durationMS int64) {
	h.marshalBroadcastAuth(daemonRunEndedMsg{
		Type:       "daemon_run_ended",
		Name:       osutil.SanitizeForLog(name, 64),
		RunID:      sanitizeHexIDForBroadcast(runID, 64),
		State:      osutil.SanitizeForLog(state, 32),
		DurationMS: durationMS,
		ErrorClass: osutil.SanitizeForLog(errClass, 64),
		Trigger:    osutil.SanitizeForLog(trigger, 32),
	})
}

// sanitizeHexIDForBroadcast returns id unchanged when it matches the
// cron.IsValidID hex shape (and fits within maxLen), otherwise routes
// through the regular sanitiser, avoiding its strings.Map slow path.
func sanitizeHexIDForBroadcast(id string, maxLen int) string {
	if len(id) <= maxLen && cron.IsValidID(id) {
		return id
	}
	return osutil.SanitizeForLog(id, maxLen)
}

// sanitizeTriggerForBroadcast short-circuits the closed cron TriggerKind enum
// (lowercase-ASCII constants the scheduler controls) so the hot cron-run
// fan-out skips SanitizeForLog's byte-scan. Anything outside the enum — e.g. a
// future externally-derived webhook trigger name — still goes through the
// sanitiser (#2232).
func sanitizeTriggerForBroadcast(trigger string) string {
	switch cron.TriggerKind(trigger) {
	case cron.TriggerScheduled, cron.TriggerManual, runtelemetry.TriggerCatchup:
		return trigger
	}
	return osutil.SanitizeForLog(trigger, 32)
}

// sanitizeSessionIDForBroadcast short-circuits canonical UUID session IDs
// (the form every cron run records); non-UUID shapes still go through the
// sanitiser (#2232).
func sanitizeSessionIDForBroadcast(sessionID string) string {
	if discovery.IsValidSessionID(sessionID) {
		return sessionID
	}
	return osutil.SanitizeForLog(sessionID, 128)
}
