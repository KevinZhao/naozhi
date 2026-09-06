// send_engine.go — sendEngine owns the send pipeline: the per-key dispatch
// queue, the spawn guard, and the accounting that lets Shutdown wait for
// in-flight send goroutines. Hub (WS entry) and SendHandler (HTTP entry) each
// hold one *sendEngine and no longer know about each other.
//
// The pipeline methods themselves stay in send.go / send_owner_loop.go with an
// (e *sendEngine) receiver — moving the bodies here would put this file over
// the package's 500-line limit, and two contract tests read "send.go" by
// filename (one of them the R175-SEC-P1 log-injection redaction gate, which
// would go silently green against a file that no longer holds sessionSend).
// See docs/rfc/send-engine-extraction.md §2.2.
package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// sendEngine is the enqueue → guard → router → TrackSend chain that both
// transports share. Construct it only with newSendEngine: a zero value is a
// hazard, not a convenience — allowedRoot "" disables validateWorkspace's
// containment check entirely (server_validate.go), a nil ctx panics inside the
// detached remote-proxy goroutine where net/http's recover cannot reach, and a
// nil guard or router panics on the legacy / attachment-fallback paths.
type sendEngine struct {
	// ── owned state (migrated off Hub) ──
	// queue is the MessageEnqueuer interface, not *dispatch.MessageQueue, so
	// tests can swap it. Only a non-nil concrete queue is boxed: send.go's
	// `e.queue == nil` legacy-fallback gate depends on a typed nil never
	// landing here (#377).
	queue MessageEnqueuer
	guard *session.Guard
	// wg tracks background send goroutines so drain can wait for them before
	// router/session state is torn down.
	wg sync.WaitGroup
	// trackMu + closed serialise a late Add(1) with drain's Wait.
	// CONTRACT: every goroutine registered on wg MUST go through TrackSend()
	// and honour its shuttingDown result; a direct wg.Add(1) can outlive
	// drain and dereference torn-down maps.
	trackMu sync.Mutex
	closed  bool
	// legacyInvokes counts sessionSend falls-through to sessionSendLegacy
	// (nil queue); production steady state must read zero (#710).
	legacyInvokes atomic.Int64

	// ── shared dependencies ──
	// INVARIANT: read-only after construction. Hub keeps its own reference to
	// each of these because the subscribe / broadcast / eventpush paths use
	// them too; both sides must point at the same instance
	// (send_engine_contract_test.go). Deduplicating them is Epic K (#2549).
	ctx         context.Context // cancelled by Hub.Shutdown to stop in-flight sends
	router      sendEngineRouter
	resolver    *session.KeyResolver
	agents      map[string]session.AgentOpts
	projectMgr  *project.Manager
	scratchPool *session.ScratchPool
	scheduler   CronView
	allowedRoot string

	// notify is the only way out to the dashboard; never nil after
	// newSendEngine (a nil interface would panic inside the owner goroutine,
	// where ownerLoop's recover would swallow it and the message would just
	// vanish).
	notify sendNotifier
}

// sendEngineOpts mirrors the subset of HubOptions the engine needs. Kept as a
// struct (not positional args) so a new dependency is a compile-time addition
// at one call site rather than a silently-zero field.
type sendEngineOpts struct {
	// Queue is the CONCRETE type, not MessageEnqueuer: taking the interface
	// here would box a nil *dispatch.MessageQueue before newSendEngine's nil
	// check ever runs, so `e.queue == nil` would read false and silently
	// disable send.go's legacy-fallback gate (#377 — reintroduced once during
	// #2551 and caught by TestNewHub_NilQueue_LeavesInterfaceFieldNil). Also
	// what consumer-interfaces.md §4.5 prescribes: keep concrete types at
	// construction time.
	Queue       *dispatch.MessageQueue
	Guard       *session.Guard
	Ctx         context.Context
	Router      sendEngineRouter
	Resolver    *session.KeyResolver
	Agents      map[string]session.AgentOpts
	ProjectMgr  *project.Manager
	ScratchPool *session.ScratchPool
	Scheduler   CronView
	AllowedRoot string
	Notify      sendNotifier
}

// newSendEngine is the only constructor. It does not reject a nil Router:
// hand-rolled test hubs go through NewHub(HubOptions{}) with no Router, and
// their send paths fail at GetOrCreate exactly as they do today. What it does
// guarantee is that ctx and notify are usable, because both have failure modes
// that are silent or fatal rather than a plain error.
func newSendEngine(o sendEngineOpts) *sendEngine {
	if o.Ctx == nil {
		// Mirrors NewHub's ParentCtx handling. context.WithTimeout(nil, …)
		// panics, and the remote-proxy call site is a detached goroutine.
		o.Ctx = context.Background()
	}
	notify := o.Notify
	// Typed-nil unwrap, same hazard as queue below: a nil *Hub boxed into the
	// interface reads non-nil, so the guard has to look at the concrete type.
	if hn, ok := notify.(*Hub); ok && hn == nil {
		notify = nil
	}
	if notify == nil {
		notify = nopNotifier{}
	}
	e := &sendEngine{
		guard:       o.Guard,
		ctx:         o.Ctx,
		router:      o.Router,
		resolver:    o.Resolver,
		agents:      o.Agents,
		projectMgr:  o.ProjectMgr,
		scratchPool: o.ScratchPool,
		scheduler:   o.Scheduler,
		allowedRoot: o.AllowedRoot,
		notify:      notify,
	}
	// A nil queue routes every send through sessionSendLegacy and loses the
	// dispatch queue's rate-limit / collect-window / passthrough modes; Error
	// level so a misconfigured production wiring is visible in journalctl.
	if o.Queue == nil {
		slog.Error("server: send engine constructed without MessageQueue; falling back to legacy guard path (dispatch queue features disabled, R-LEGACY-SEND blocker)")
	} else {
		e.queue = o.Queue
	}
	return e
}

// TrackSend reserves a wg slot for a background send goroutine and returns a
// release function plus a shuttingDown flag. When shuttingDown is true the
// caller MUST NOT spawn the goroutine.
func (e *sendEngine) TrackSend() (release func(), shuttingDown bool) {
	e.trackMu.Lock()
	defer e.trackMu.Unlock()
	if e.closed {
		return func() {}, true
	}
	e.wg.Add(1)
	return e.wg.Done, false
}

// drain closes the send admission window and waits for the goroutines
// registered through TrackSend. Idempotent: closed is a monotonic bool under
// trackMu and WaitGroup.Wait tolerates concurrent callers.
//
// Barrier semantics: a racing TrackSend lands on one side of the closed store;
// afterwards nobody Adds, so wg.Wait cannot be escaped.
//
// It does NOT cover Server.sendWithBroadcast (send.go) — the IM / cron entry is
// a synchronous call that never registers on wg.
//
// CALL-SITE PRECONDITIONS (all three are hard; violating one either hangs
// Shutdown forever or lets a broadcast run past it, and -race cannot see
// either because neither is a data race):
//
//  1. h.cancel() has been called. Otherwise wg.Wait blocks for the full
//     remote-RPC timeout (60s in dashboard_send.go, remoteNodeProxyTimeout for
//     the WS path) instead of returning as soon as ctx is cancelled.
//  2. debounceClosed AND debounceClosedFast are already published. The waited
//     goroutines call BroadcastSessionsUpdate through sendNotifier, which does
//     clientWG.Add(1) to arm the debounce timer — a send goroutine can enlarge
//     clientWG. Draining before the window is shut arms a callback that runs
//     after Shutdown has emptied authClientsSlice.
//  3. The caller holds NONE of h.mu / authMu / debounceMu. The waited
//     goroutines re-enter all three through sendNotifier
//     (BroadcastSessionReady → authMu.RLock, broadcastState → h.mu.RLock,
//     BroadcastSessionsUpdate → debounceMu.Lock). Calling drain inside the
//     debounceMu critical section is a deterministic deadlock: the in-flight
//     goroutine is already past the debounceClosedFast fast path and blocked
//     on debounceMu.Lock while Shutdown holds it waiting for wg.
//
// It must also run before the node connections are closed, so an in-flight
// remote RPC cannot write to a closed nc.conn.
func (e *sendEngine) drain() {
	e.trackMu.Lock()
	e.closed = true
	e.trackMu.Unlock()
	e.wg.Wait()
}

// sendErrorCallback adapts broadcastSendError to the sessionSend onAsyncError
// signature for the HTTP send path (was Hub.httpSendErrorCallback).
//
// Informational outcomes are dropped: this callback fans out to every
// subscriber of the key, so if A's HTTP send is aborted by B's /urgent, B's tab
// would otherwise tear down its own optimistic bubble. session_state settles
// the UI instead; real failures still fan out.
func (e *sendEngine) sendErrorCallback(key string) asyncErrorFn {
	return func(err error, errMsg string) {
		if informationalSendErr(err) {
			return
		}
		e.notify.broadcastSendError(key, errMsg)
	}
}

// nopNotifier absorbs broadcasts for an engine built without a Hub, so the
// engine itself carries no nil checks on the send hot path.
type nopNotifier struct{}

func (nopNotifier) BroadcastSessionReady(string)          {}
func (nopNotifier) BroadcastSessionsUpdate()              {}
func (nopNotifier) broadcastState(string, string, string) {}
func (nopNotifier) broadcastSendError(string, string)     {}
