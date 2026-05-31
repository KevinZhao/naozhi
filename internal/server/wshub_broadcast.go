// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     broadcast block (debounceMu / debounceTimer / debounceFirst /
//	            debounceClosed / debounceClosedFast / debounceFire) +
//	            subscriber block (clients) for SendRaw fanout
//	READS:      shared deps block (read-only after ctor) + send block
//	            (queue / droppedTotal for broadcast-aware enqueue)
//
// Phase 4b 起 rule 3b 升级到 AST 字段访问对账时，会校验本文件方法体
// 的字段访问匹配本契约；当前 Phase 0b 仅 marker 存在性。
package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
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
//   - BroadcastCronRunStarted / BroadcastCronRunEnded and their named
//     WS payload structs (cron_result wire frame deleted in Phase D —
//     see cronRunStartedMsg / cronRunEndedMsg comment block below)
//   - BroadcastDaemonRunStarted / BroadcastDaemonRunEnded
//   - sanitizeHexIDForBroadcast (Job/Run hex-ID short-circuit)
//   - DroppedMessages (atomic-counter accessor, broadcast-adjacent)
//
// All Hub state used by these helpers stays on *Hub: debounceMu /
// debounceTimer / debounceFirst / debounceClosed / clients / mu /
// droppedTotal. This file is pure code-relocation — no behaviour change.

// Pre-marshaled static message body, derived ONCE from node.ServerMsg at
// package init so it can never drift from the wire schema. R243-ARCH-24
// (#869) incremental slice: the previous hand-written byte literal was a
// second, unverified source of truth for the sessions_update frame and
// carried a "must stay exactly in sync" comment — exactly the scattered-
// wire-struct hazard the issue flags. Deriving from the struct keeps the
// zero-alloc broadcast hot path (still a single shared []byte) while making
// node.ServerMsg the only authority. All fields are omitempty so the result
// is `{"type":"sessions_update"}`; marshalSessionsUpdate panics on the
// impossible Marshal error rather than shipping a malformed/empty frame.
var sessionsUpdateMsg = marshalSessionsUpdate()

func marshalSessionsUpdate() []byte {
	data, err := json.Marshal(node.ServerMsg{Type: "sessions_update"})
	if err != nil {
		panic("server: marshal sessions_update frame: " + err.Error())
	}
	return data
}

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

// releaseBroadcastSnap returns a client snapshot to broadcastClientSnapPool
// after a fan-out completes. It (1) nils every *wsClient slot so a
// disconnected client can be GC'd before the backing array is reused
// (R59-PERF-L1), and (2) drops oversized backing arrays rather than pinning
// them in the pool — replacing them with a fresh small slice so a single
// "big broadcast" cannot permanently deplete a pool slot (R58-PERF-005).
//
// R243-ARCH-2 (#459 server-domain slice): broadcastToAuthenticated and
// broadcastSessionSystemEvent repeated this identical clear→cap-check→Put
// tail; consolidating it keeps the two fan-out paths' pool discipline in
// one place so a future tweak (e.g. the cap threshold) can't drift between
// them. Pure code-relocation — no behaviour change.
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
// Takes a pointer snapshot under RLock and releases the lock before the per-
// client channel sends. SendRaw itself is non-blocking, but with hundreds of
// clients a loop under RLock still serialises `register`/`unregister` behind
// every broadcast; snapshotting removes that contention amplifier and the
// backing slice is reused via sync.Pool so steady-state broadcasts are zero-alloc.
//
// R040034-PERF-23 (#1409): iterate the h.authClients mirror instead of
// scanning h.clients with a per-element c.authenticated.Load() filter.
// cron's run-started/ended fan-out paid one O(N_clients) walk per broadcast
// wave even when most connections were still pre-auth (no-token mode is the
// only path that bypasses handleAuth — tokens routes flip authenticated only
// after the inner auth message exchanges). The mirror is updated under h.mu
// alongside h.clients (register / markAuthenticated / unregister / Shutdown)
// so the two stay consistent. Hand-rolled test hubs that bypass NewHub leave
// h.authClients nil; the nil-guard falls back to the legacy filter loop so
// pre-existing fixtures observe no behaviour change.
func (h *Hub) broadcastToAuthenticated(data []byte) {
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]

	h.mu.RLock()
	if h.authClients != nil {
		for c := range h.authClients {
			snap = append(snap, c)
		}
	} else {
		// Legacy fallback for hand-rolled hubs that do not initialise
		// authClients. Production hubs always go through NewHub.
		for c := range h.clients {
			if c.authenticated.Load() {
				snap = append(snap, c)
			}
		}
	}
	h.mu.RUnlock()

	for _, c := range snap {
		c.SendRaw(data)
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// marshalBroadcastAuth consolidates the marshal→err-guard→fan-out tail shared
// by every "broadcast to all authenticated clients" producer (session_state,
// cron/daemon run-started/ended). R243-ARCH-15 (#845) incremental slice: these
// six call sites previously repeated the identical `data, err := marshalPooled
// (v); if err != nil { return }; h.broadcastToAuthenticated(data)` triple. A
// marshal failure is swallowed (matching the prior behaviour) because the WS
// payload structs are fixed-shape and cannot fail json.Marshal in practice;
// dropping the frame is preferable to panicking the producer goroutine.
func (h *Hub) marshalBroadcastAuth(v any) {
	data, err := marshalPooled(v)
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
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
	// R246-PERF-9 / #723: check the atomic mirror BEFORE acquiring the
	// debounce mutex. Once Shutdown has set the flag every subsequent call
	// is a no-op; without this fast path, the dozens of producer paths
	// racing a teardown (router/cron/dashboard send/scratch/etc.) all
	// serialise on debounceMu just to read the bool and return. The
	// authoritative `debounceClosed` check below still runs under the
	// mutex so the Shutdown ordering contract is unchanged for callers
	// that arrive before the flag publishes.
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

// cronRunStartedMsg / cronRunEndedMsg are the cron-run-history WS payloads.
// Phase D (RFC §3.5) deleted the legacy cronResultMsg / BroadcastCronResult
// pair: dashboard.js's cron_result subscription was a strict subset of its
// cron_run_ended subscription (announce + fetchCronJobs + renderCronPanel),
// and result text is fetched via /api/cron/jobs/<id>/runs/<runID> when
// needed rather than carried inline on the success-path WS frame. The
// announce("定时任务已完成") moved to dashboard.js's cron_run_ended
// succeeded branch.
//
// New clients subscribe to the run-started / run-ended pair, which
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
	h.marshalBroadcastAuth(cronRunStartedMsg{
		Type:      "cron_run_started",
		JobID:     sanitizeHexIDForBroadcast(jobID, 64),
		RunID:     sanitizeHexIDForBroadcast(runID, 64),
		StartedAt: startedAt.UnixMilli(),
		Trigger:   osutil.SanitizeForLog(trigger, 32),
		SessionID: osutil.SanitizeForLog(sessionID, 128),
		Fresh:     fresh,
	})
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
	h.marshalBroadcastAuth(cronRunEndedMsg{
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
}

// broadcastSessionSystemEvent pushes a synthetic `system`-type event frame to
// every authenticated client currently subscribed to `key`. R176-ARCH-NX
// (#433): the primary→remote send path previously surfaced a remote-send
// failure ONLY to the originating client via send_ack, while the symmetric
// remote→primary path (upstream/connector_rpc.go) injects a LogSystemEvent that
// reaches every dashboard subscribed to the session. This restores parity for
// remote sessions, whose EventLog lives on the remote node and so cannot be
// appended to locally — instead we fan the failure out over the same WS
// `event` frame that streamed remote events already use, scoped to subscribers
// of that key so it is not cross-tenant noise.
//
// summary is caller-sanitised (osutil.SanitizeForLog) before reaching here —
// it is broadcast verbatim to subscribed dashboards and would otherwise be a
// log/terminal-injection primitive, mirroring the connector_rpc.go contract.
func (h *Hub) broadcastSessionSystemEvent(key, summary string) {
	if key == "" || summary == "" {
		return
	}
	// Snapshot the session's subscribers BEFORE marshalling. Remote/background
	// sessions frequently have nobody watching when a send/interrupt fails —
	// in that case there is nothing to deliver, so paying the marshalPooled
	// reflect cost + a pooled-buffer round trip would be pure waste. The
	// subscriber scan is cheap (one map lookup per authenticated client) and
	// the common no-subscriber case now returns before any allocation.
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]
	h.mu.RLock()
	if h.authClients != nil {
		for c := range h.authClients {
			if _, ok := c.subscriptions[key]; ok {
				snap = append(snap, c)
			}
		}
	} else {
		for c := range h.clients {
			if !c.authenticated.Load() {
				continue
			}
			if _, ok := c.subscriptions[key]; ok {
				snap = append(snap, c)
			}
		}
	}
	h.mu.RUnlock()

	if len(snap) > 0 {
		ev := cli.EventEntry{
			Time:    time.Now().UnixMilli(),
			Type:    "system",
			Summary: summary,
		}
		if data, err := marshalPooled(node.ServerMsg{Type: "event", Key: key, Event: &ev}); err == nil {
			for _, c := range snap {
				c.SendRaw(data)
			}
		}
	}

	releaseBroadcastSnap(snapPtr, snap)
}

// DroppedMessages returns the total number of messages dropped across all
// clients since the process started. Lock-free atomic load; see the struct
// field comment for why this replaced a per-client RLock scan.
func (h *Hub) DroppedMessages() int64 {
	return h.droppedTotal.Load()
}

// LegacySendInvokes returns the total number of times sessionSend fell
// through to the deprecated sessionSendLegacy path. Production Hubs wire a
// real MessageQueue and never increment this counter; tests/headless tools
// that omit Queue do. R-LEGACY-SEND (#710) uses this counter to drive the
// migration to one delivery contract — once every test fixture wires a
// MessageQueue stub the counter stays at zero and sessionSendLegacy can
// be deleted alongside its sole caller branch in send.go.
func (h *Hub) LegacySendInvokes() int64 {
	if h == nil {
		return 0
	}
	return h.legacySendInvokes.Load()
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
// through the regular sanitiser. Avoids the strings.Map slow-path on
// the common case where Job/Run IDs are produced by generateHexID.
// R222-PERF-15.
func sanitizeHexIDForBroadcast(id string, maxLen int) string {
	if len(id) <= maxLen && cron.IsValidID(id) {
		return id
	}
	return osutil.SanitizeForLog(id, maxLen)
}
