package server

import (
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
)

// File: wshub_broadcast.go
//
// Broadcast and fan-out helpers extracted from wshub.go (R243-ARCH-2 split).
// Owns:
//   - broadcastClientSnapPool: pooled []*wsClient backing array
//   - sessionsUpdateMsg: pre-marshaled byte literal
//   - broadcastToAuthenticated / broadcastState / BroadcastSessionReady
//   - BroadcastSessionsUpdate (debounced) + doBroadcastSessionsUpdate
//   - BroadcastCronResult / BroadcastCronRunStarted / BroadcastCronRunEnded
//     and their named WS payload structs
//   - BroadcastDaemonRunStarted / BroadcastDaemonRunEnded
//   - sanitizeHexIDForBroadcast (Job/Run hex-ID short-circuit)
//   - DroppedMessages (atomic-counter accessor, broadcast-adjacent)
//
// All Hub state used by these helpers stays on *Hub: debounceMu /
// debounceTimer / debounceFirst / debounceClosed / clients / mu /
// droppedTotal. This file is pure code-relocation — no behaviour change.

// Pre-marshaled static message body. A plain byte literal avoids paying
// a json.Marshal on package init and removes the nominal panic branch
// (the struct has only a Type string field so Marshal cannot fail). The
// shape must stay exactly in sync with node.ServerMsg JSON encoding.
var sessionsUpdateMsg = []byte(`{"type":"sessions_update"}`)

// broadcastClientSnapPool reuses the []*wsClient backing array across
// broadcasts so high-frequency session_state / sessions_update traffic does
// not allocate one slice per broadcast. The drop threshold is keyed off
// maxWSConns so a steady-state fleet at any size up to the ceiling keeps
// pooling; only genuinely oversized slices (e.g. after a brief spike over
// the cap) fall through to a fresh allocation.
const broadcastSnapPoolMaxCap = maxWSConns

var broadcastClientSnapPool = sync.Pool{
	New: func() any {
		s := make([]*wsClient, 0, 32)
		return &s
	},
}

// broadcastToAuthenticated sends raw data to all authenticated WebSocket clients.
// Takes a pointer snapshot under RLock and releases the lock before the per-
// client channel sends. SendRaw itself is non-blocking, but with hundreds of
// clients a loop under RLock still serialises `register`/`unregister` behind
// every broadcast; snapshotting removes that contention amplifier and the
// backing slice is reused via sync.Pool so steady-state broadcasts are zero-alloc.
func (h *Hub) broadcastToAuthenticated(data []byte) {
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]

	h.mu.RLock()
	for c := range h.clients {
		if c.authenticated.Load() {
			snap = append(snap, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range snap {
		c.SendRaw(data)
	}
	// Clear *wsClient pointers so a disconnected client can be GC'd before
	// the snap slice is returned to the caller / dropped. Clearing happens
	// on both paths: for the pool-eligible path it prevents stale pointers
	// surviving in the pooled backing array until the next Get; for the
	// oversized path it releases references now instead of waiting for the
	// long-lived parent goroutine's stack frame to unwind. R59-PERF-L1.
	for i := range snap {
		snap[i] = nil
	}
	// Oversized snapshots (e.g. after a one-off broadcast to 5000 clients)
	// would pin an arbitrarily large backing array if returned to the pool.
	// Drop the slice but still return the pointer header with a fresh small
	// backing array so the pool slot is not permanently depleted — otherwise
	// a single "big broadcast" would shrink the pool by one slot until
	// process exit. R58-PERF-005.
	if cap(snap) <= broadcastSnapPoolMaxCap {
		*snapPtr = snap[:0]
	} else {
		*snapPtr = make([]*wsClient, 0, 32)
	}
	broadcastClientSnapPool.Put(snapPtr)
}

// broadcastState sends a session_state message to ALL authenticated clients.
// This mirrors BroadcastSessionReady: the "running" start is sent to everyone,
// so the final state must also reach everyone — otherwise clients not subscribed
// to this session would see a stale "running" dot in the sidebar forever.
func (h *Hub) broadcastState(key, state, reason string) {
	data, err := marshalPooled(node.ServerMsg{Type: "session_state", Key: key, State: state, Reason: reason})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// BroadcastSessionReady sends a session_state "running" to ALL authenticated clients
// so they can auto-subscribe. Unlike broadcastState, this is not limited to already-
// subscribed clients — needed for new sessions where nobody is subscribed yet.
func (h *Hub) BroadcastSessionReady(key string) {
	data, err := marshalPooled(node.ServerMsg{Type: "session_state", Key: key, State: "running"})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
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
	if h.debounceTimer != nil {
		if now.Sub(h.debounceFirst) >= maxDebounceDelay {
			// Hard cap reached — let the pending timer fire without resetting.
			return
		}
		// time.Timer.Reset on a timer whose AfterFunc already fired but whose
		// callback is still blocked on debounceMu would schedule a SECOND run
		// without a matching clientWG.Add — breaking the Shutdown Wait and
		// producing a negative clientWG count. Stop() returns false if the
		// callback already ran or is scheduled to run; in that case we treat
		// the in-flight callback as the one that will do the broadcast and
		// skip rescheduling. The callback clears debounceTimer under
		// debounceMu, so subsequent calls will start a fresh timer.
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
	// R239-PERF-6: reuse the pre-bound closure stored on the Hub instead
	// of allocating a fresh `func()` literal per call. AfterFunc itself
	// still allocates a *time.Timer (unavoidable without an internal pool),
	// but the captured-variable + funcval pair is now amortised across
	// every refresh on the same Hub. Hand-rolled hubs from old tests that
	// skip NewHub fall back to building the callback inline. The fallback
	// keeps the R249-GO-7 closed-check so Shutdown-races stay safe.
	fire := h.debounceFire
	if fire == nil {
		fire = func() {
			defer h.clientWG.Done()
			h.debounceMu.Lock()
			h.debounceTimer = nil
			closed := h.debounceClosed
			h.debounceMu.Unlock()
			if closed {
				return
			}
			h.doBroadcastSessionsUpdate()
		}
	}
	h.debounceTimer = time.AfterFunc(debounceInterval, fire)
}

func (h *Hub) doBroadcastSessionsUpdate() {
	data := sessionsUpdateMsg
	h.broadcastToAuthenticated(data)
}

// cronResultMsg is the WS payload broadcast on cron job completion. Declared
// as a named type (not an inline anonymous struct) so json/reflect caches the
// type descriptor once across all calls.
type cronResultMsg struct {
	Type   string `json:"type"`
	JobID  string `json:"job_id,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// BroadcastCronResult notifies all connected WS clients that a cron job completed.
func (h *Hub) BroadcastCronResult(jobID, result, errMsg string) {
	// R185-SEC-H2: scheduler generates jobID as 8-char hex today, but if a
	// future path ever surfaces a config-supplied / user-typed ID, bidi/C0
	// chars would reach the dashboard via a SetEscapeHTML(false) encoder.
	// Sanitize defensively; result/errMsg are already scrubbed at recordResult.
	data, err := marshalPooled(cronResultMsg{
		Type:   "cron_result",
		JobID:  osutil.SanitizeForLog(jobID, 64),
		Result: result,
		Error:  errMsg,
	})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// cronRunStartedMsg / cronRunEndedMsg are P0 cron-run-history (RFC §7.2)
// WS payloads. cron_result is preserved on the success path for backward
// compatibility (clients that haven't migrated still see the result text);
// new clients should subscribe to the run-started / run-ended pair, which
// covers every terminal state including skipped/canceled where cron_result
// historically did not fire.
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
	// R222-PERF-15: jobID / runID are produced by cron.generateHexID — pure
	// lowercase hex of fixed length. cron.IsValidID rejects anything else,
	// so once it returns true we can skip SanitizeForLog (slow path allocates
	// a strings.Map output even on the no-op branch when len > 0). Untrusted
	// or shape-mismatched input still falls through to the sanitiser.
	data, err := marshalPooled(cronRunStartedMsg{
		Type:      "cron_run_started",
		JobID:     sanitizeHexIDForBroadcast(jobID, 64),
		RunID:     sanitizeHexIDForBroadcast(runID, 64),
		StartedAt: startedAt.UnixMilli(),
		Trigger:   osutil.SanitizeForLog(trigger, 32),
		SessionID: osutil.SanitizeForLog(sessionID, 128),
		Fresh:     fresh,
	})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// BroadcastCronRunEnded emits cron_run_ended for every terminal state
// (succeeded / failed / skipped / timed_out / canceled). The dashboard
// uses State to decide colour and whether to refetch the list (counters
// updated). errorMsg is already path-redacted + sanitised by the cron
// package's recordResultP0 → SanitizeForLog pipeline.
func (h *Hub) BroadcastCronRunEnded(jobID, runID, state string, startedAt, endedAt time.Time, durationMS int64, sessionID, errClass, errMsg, trigger string) {
	// errClass/trigger are typed enums today (cron.ErrorClass / cron.TriggerKind)
	// so currently safe; defensive sanitisation matches the treatment of jobID
	// and shields a future code path that derives them from external config
	// (e.g. webhook trigger names) from log/payload injection. R221-FIX-P2-7.
	// R222-PERF-15: see sanitizeHexIDForBroadcast — hex IDs short-circuit.
	data, err := marshalPooled(cronRunEndedMsg{
		Type:       "cron_run_ended",
		JobID:      sanitizeHexIDForBroadcast(jobID, 64),
		RunID:      sanitizeHexIDForBroadcast(runID, 64),
		State:      state,
		StartedAt:  startedAt.UnixMilli(),
		EndedAt:    endedAt.UnixMilli(),
		DurationMS: durationMS,
		SessionID:  osutil.SanitizeForLog(sessionID, 128),
		ErrorClass: osutil.SanitizeForLog(errClass, 64),
		ErrorMsg:   errMsg,
		Trigger:    osutil.SanitizeForLog(trigger, 32),
	})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// DroppedMessages returns the total number of messages dropped across all
// clients since the process started. Lock-free atomic load; see the struct
// field comment for why this replaced a per-client RLock scan.
func (h *Hub) DroppedMessages() int64 {
	return h.droppedTotal.Load()
}

// daemonRunStartedMsg / daemonRunEndedMsg are the WS payloads for
// docs/rfc/system-session.md §9.4.  Crucially we do NOT carry an
// ErrorMsg field — error messages from the daemon's Runner subprocess
// can echo back portions of the user-supplied prompt (CLI "context too
// long" failures are a known case), and broadcasting that to every
// authenticated dashboard client constitutes cross-tenant leakage.
// Server-side slog still carries the full error for operator-side
// debugging.
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

// BroadcastDaemonRunStarted emits daemon_run_started.
//
// name / runID / trigger / state come from compiled-in enums (not
// operator-supplied), but we run them through SanitizeForLog anyway as
// defence-in-depth — a future caller might wire a daemon name from
// config or pass through external content; sanitising at the broadcast
// boundary keeps us safe regardless.
func (h *Hub) BroadcastDaemonRunStarted(name, runID, trigger string, startedAt time.Time) {
	data, err := marshalPooled(daemonRunStartedMsg{
		Type:      "daemon_run_started",
		Name:      osutil.SanitizeForLog(name, 64),
		RunID:     sanitizeHexIDForBroadcast(runID, 64),
		Trigger:   osutil.SanitizeForLog(trigger, 32),
		StartedAt: startedAt.UnixMilli(),
	})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// BroadcastDaemonRunEnded emits daemon_run_ended.  ErrorMsg is
// intentionally absent — see daemonRunEndedMsg above.
func (h *Hub) BroadcastDaemonRunEnded(name, runID, state, errClass, trigger string, durationMS int64) {
	data, err := marshalPooled(daemonRunEndedMsg{
		Type:       "daemon_run_ended",
		Name:       osutil.SanitizeForLog(name, 64),
		RunID:      sanitizeHexIDForBroadcast(runID, 64),
		State:      osutil.SanitizeForLog(state, 32),
		DurationMS: durationMS,
		ErrorClass: osutil.SanitizeForLog(errClass, 64),
		Trigger:    osutil.SanitizeForLog(trigger, 32),
	})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// sanitizeHexIDForBroadcast returns id unchanged when it matches the
// cron.IsValidID hex shape (and fits within maxLen), otherwise routes
// through the regular sanitiser. Avoids the strings.Map slow-path on
// the common case where Job/Run IDs are produced by generateHexID.
// R222-PERF-15.
func sanitizeHexIDForBroadcast(id string, maxLen int) string {
	if len(id) <= maxLen && cron.IsValidID(id) {
		return id
	}
	return osutil.SanitizeForLog(id, maxLen)
}
