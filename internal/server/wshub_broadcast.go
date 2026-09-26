package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// sessionsUpdateMsg is the pre-marshaled sessions_update frame, derived from
// node.ServerMsg at package init so it cannot drift from the wire schema while
// keeping the broadcast hot path zero-alloc (#869).
var sessionsUpdateMsg = marshalSessionsUpdate()

func marshalSessionsUpdate() []byte {
	data, err := json.Marshal(wsproto.NewSessionsUpdate())
	if err != nil {
		panic("server: marshal sessions_update frame: " + err.Error())
	}
	return data
}

// broadcastSnapPoolMaxCap: snapshots grown past the connection ceiling are not
// returned to broadcastClientSnapPool, so a spike cannot pin an oversized array.
const broadcastSnapPoolMaxCap = maxWSConns

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
// The recipient snapshot is taken before the per-client SendRaw loop so
// register / unregister never serialise behind a broadcast.
func (h *Hub) broadcastToAuthenticated(data []byte) {
	snapPtr, snap := h.snapshotAuthenticated()
	for _, c := range snap {
		c.SendRaw(data)
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// snapshotAuthenticated returns a pooled snapshot of the authenticated
// clients. The caller MUST return it via releaseBroadcastSnap once the
// fan-out completes.
func (h *Hub) snapshotAuthenticated() (*[]*wsClient, []*wsClient) {
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	return snapPtr, h.subs.authenticated((*snapPtr)[:0])
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
	h.marshalBroadcastAuth(wsproto.NewSessionState(wsproto.SessionState{Key: key, State: state, Reason: reason}))
}

// BroadcastSessionReady sends a session_state "running" to ALL authenticated clients
// so they can auto-subscribe. Unlike broadcastState, this is not limited to already-
// subscribed clients — needed for new sessions where nobody is subscribed yet.
func (h *Hub) BroadcastSessionReady(key string) {
	h.marshalBroadcastAuth(wsproto.NewSessionState(wsproto.SessionState{Key: key, State: "running"}))
}

// BroadcastSessionsUpdate asks for a sessions_update broadcast. Bursts
// coalesce: the broadcast fires debounceInterval after the last call, and no
// later than maxDebounceDelay after the first, so a sustained burst still
// refreshes clients.
func (h *Hub) BroadcastSessionsUpdate() {
	h.debounce.trigger()
}

func (h *Hub) doBroadcastSessionsUpdate() {
	data := sessionsUpdateMsg
	h.broadcastToAuthenticated(data)
}

// BroadcastRunStarted emits the subsystem-neutral run_started frame (#2540).
// Called through hubBroadcaster from every run producer's lifecycle hook.
// Sanitisation is uniform: sanitizeHexIDForBroadcast fast-paths the hex IDs
// cron produces and falls back to SanitizeForLog for anything else — which is
// exactly the treatment daemon names (compiled-in, but defence-in-depth
// against a future config-derived producer) got from their dedicated frame.
func (h *Hub) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	rec := ev.Record()
	h.marshalBroadcastAuth(wsproto.NewRunStarted(wsproto.RunStarted{
		Subsystem: string(rec.Subsystem),
		OwnerID:   sanitizeHexIDForBroadcast(rec.OwnerID, 64),
		RunID:     sanitizeHexIDForBroadcast(rec.RunID, 64),
		StartedAt: rec.StartedAt.UnixMilli(),
		Trigger:   sanitizeTriggerForBroadcast(string(rec.Trigger)),
		SessionID: sanitizeSessionIDForBroadcast(rec.SessionID),
		Fresh:     rec.Fresh,
	}))
}

// BroadcastRunEnded emits run_ended for every terminal state (succeeded /
// failed / skipped / timed_out / canceled). The dashboard uses State to decide
// colour and whether to refetch the producing subsystem's list.
//
// SECURITY: ErrorMsg goes on the wire only for subsystems whose policy allows
// it. cron passes it through (already path-redacted + SanitizeForLog'd by
// recordResultP0); sysession's is dropped before this method is reached — see
// hubBroadcaster.BroadcastRunEnded, which owns that policy.
func (h *Hub) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	rec := ev.Record()
	h.marshalBroadcastAuth(wsproto.NewRunEnded(wsproto.RunEnded{
		Subsystem:  string(rec.Subsystem),
		OwnerID:    sanitizeHexIDForBroadcast(rec.OwnerID, 64),
		RunID:      sanitizeHexIDForBroadcast(rec.RunID, 64),
		State:      osutil.SanitizeForLog(string(rec.State), 32),
		StartedAt:  rec.StartedAt.UnixMilli(),
		EndedAt:    rec.EndedAt.UnixMilli(),
		DurationMS: rec.DurationMS,
		SessionID:  sanitizeSessionIDForBroadcast(rec.SessionID),
		ErrorClass: osutil.SanitizeForLog(string(rec.ErrorClass), 64),
		ErrorMsg:   osutil.SanitizeForLog(rec.ErrorMsg, 512),
		Trigger:    sanitizeTriggerForBroadcast(string(rec.Trigger)),
	}))
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
		return wsproto.NewEvent(wsproto.Event{Key: key, Event: &ev})
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
	// A nil receiver reads 0: package callers may probe a not-yet-built Hub
	// through an interface, and R-LEGACY-SEND tooling depends on it.
	if h == nil {
		return 0
	}
	return h.engine.legacyInvokes.Load()
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
	if claudefs.IsValidSessionID(sessionID) {
		return sessionID
	}
	return osutil.SanitizeForLog(sessionID, 128)
}
