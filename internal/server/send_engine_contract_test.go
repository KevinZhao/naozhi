package server

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// newSendEngineForTest builds an engine the way tests should: through the real
// constructor, so the ctx / notify defaults are installed. Never build a
// sendEngine as a struct literal — a zero value is a hazard, not a shortcut
// (nil ctx panics inside a detached goroutine, allowedRoot "" disables
// validateWorkspace's containment check, a nil notify panics inside the owner
// goroutine where ownerLoop's recover swallows it and the message just
// vanishes). RFC send-engine-extraction §2.7.
func newSendEngineForTest(o sendEngineOpts) *sendEngine { return newSendEngine(o) }

// TestSendEngine_TrackSendRefusesAfterDrain pins the shutdown barrier that used
// to be four inline statements on Hub (sendTrackMu / sendClosed / sendWG). A
// TrackSend that slips through after drain returns is a goroutine running
// against torn-down router/session state.
func TestSendEngine_TrackSendRefusesAfterDrain(t *testing.T) {
	t.Parallel()
	e := newSendEngineForTest(sendEngineOpts{})

	release, shuttingDown := e.TrackSend()
	if shuttingDown {
		t.Fatal("fresh engine: TrackSend reported shuttingDown, want false")
	}

	// drain must actually wait for the outstanding slot.
	drained := make(chan struct{})
	go func() {
		e.drain()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("drain returned while a TrackSend slot was still held — wg.Wait was escaped")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain hung past 5s after the slot was released")
	}

	if _, shuttingDown := e.TrackSend(); !shuttingDown {
		t.Error("post-drain TrackSend reported shuttingDown=false — a send arriving during shutdown would escape the barrier")
	}
	// Idempotent: a second drain must not block or panic (Hub.Shutdown is
	// once-only today, but nothing in the type enforces that).
	e.drain()
}

// TestSendEngine_TrackSendRacesDrain is the -race probe for the trackMu barrier:
// N concurrent TrackSend callers against one drain. Every caller must either
// get a real slot (and release it) or be refused; none may Add after drain's
// Wait has started.
func TestSendEngine_TrackSendRacesDrain(t *testing.T) {
	t.Parallel()
	e := newSendEngineForTest(sendEngineOpts{})

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, shuttingDown := e.TrackSend()
			if shuttingDown {
				return
			}
			release()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.drain()
	}()
	wg.Wait()
}

// TestSendEngine_QueueTypedNilGate pins the #377 typed-nil hazard at its new
// home. send.go gates the legacy guard path on `e.queue == nil`; boxing a nil
// concrete *dispatch.MessageQueue into the interface field would make that read
// false and silently disable the gate.
func TestSendEngine_QueueTypedNilGate(t *testing.T) {
	t.Parallel()
	if e := newSendEngineForTest(sendEngineOpts{}); e.queue != nil {
		t.Errorf("engine built without Queue: queue = %v, want a nil interface", e.queue)
	}
	// The shape that actually bites: a nil CONCRETE pointer. This is why
	// sendEngineOpts.Queue is *dispatch.MessageQueue and not MessageEnqueuer —
	// an interface-typed opt field would box this before the constructor's nil
	// check runs. Passing it through the concrete field must still leave the
	// interface field nil.
	var nilQueue *dispatch.MessageQueue
	if e := newSendEngineForTest(sendEngineOpts{Queue: nilQueue}); e.queue != nil {
		t.Errorf("engine built with a nil *dispatch.MessageQueue: queue = %v, want a nil interface — send.go's legacy-fallback gate is disabled", e.queue)
	}
	q := dispatch.NewMessageQueueWithMode(5, 0, dispatch.ModeCollect)
	if e := newSendEngineForTest(sendEngineOpts{Queue: q}); e.queue == nil {
		t.Error("engine built with a real Queue: queue is nil")
	}
}

// TestSendEngine_NotifyNeverNil pins the two ways notify could end up nil: not
// passed at all, or passed as a typed-nil *wsBroadcaster (which reads non-nil
// through the interface). Either one panics on the first broadcast — inside the owner
// goroutine, where ownerLoop's recover turns it into a silently dropped message
// plus one log line.
func TestSendEngine_NotifyNeverNil(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]sendEngineOpts{
		"absent":    {},
		"typed-nil": {Notify: (*wsBroadcaster)(nil)},
	} {
		e := newSendEngineForTest(o)
		if e.notify == nil {
			t.Fatalf("%s: notify is a nil interface", name)
		}
		// Must not panic.
		e.notify.BroadcastSessionReady("k")
		e.notify.BroadcastSessionsUpdate()
		e.notify.broadcastState("k", "running", "")
		e.notify.broadcastSendError("k", "boom")
	}
}

// TestNewHub_SharesDependenciesWithEngine is the twin of
// hub_shared_state_test.go's nodes-registry assertion, for the Hubs tests
// build through newHubForTest. The engine keeps its own reference to each
// shared dependency instead of a *Hub back-pointer (RFC §2.1), which is only
// safe while both sides point at the SAME instance — otherwise a value the Hub
// path observes and one the send path observes can drift, and the bug shows
// up as "the send used the wrong workspace/profile". The production stack is
// TestBuildServer_SharesOneWSStack's.
func TestNewHub_SharesDependenciesWithEngine(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	resolver := &session.KeyResolver{}
	agents := map[string]session.AgentOpts{"a": {}}
	projectMgr := &project.Manager{}
	hub := newHubForTest(HubOptions{
		Router:      router,
		Resolver:    resolver,
		AllowedRoot: "/tmp/nz-root",
	}, sendEngineOpts{Guard: guard, Agents: agents, ProjectMgr: projectMgr})
	t.Cleanup(hub.Shutdown)

	if hub.engine == nil {
		t.Fatal("NewHub did not build the send engine")
	}
	if hub.engine.router != HubRouter(router) {
		t.Error("engine.router is not the Hub's router instance")
	}
	if hub.engine.resolver != hub.resolver {
		t.Error("engine.resolver is not the Hub's resolver instance")
	}
	if hub.engine.guard != guard {
		t.Error("engine.guard is not the guard passed to NewHub")
	}
	if reflect.ValueOf(hub.engine.agents).UnsafePointer() != reflect.ValueOf(agents).UnsafePointer() {
		t.Error("engine.agents is not the agent map passed to NewHub")
	}
	if hub.engine.projectMgr != projectMgr {
		t.Error("engine.projectMgr is not the project manager passed to NewHub")
	}
	if hub.engine.allowedRoot != hub.tailers.allowedRoot {
		t.Errorf("engine.allowedRoot = %q, tailers.allowedRoot = %q — a path allowed on one side would not be on the other",
			hub.engine.allowedRoot, hub.tailers.allowedRoot)
	}
	if hub.engine.notify != sendNotifier(hub.bcast) {
		t.Error("engine.notify is not the Hub's broadcaster — send-path session_state / send_error frames would reach a different client set")
	}
	// The send-block fields must be gone from Hub: the whole point of #2551.
	// Enforced structurally by the build (they no longer exist), so this only
	// documents the intent for the next reader.
	if got := hub.engine.LegacySendInvokes(); got != 0 {
		t.Errorf("fresh engine LegacySendInvokes = %d, want 0", got)
	}
}

// TestNewHub_BroadcasterSharesRegistry pins that the broadcaster fans out to
// the Hub's own subscriber registry. A broadcaster over a fresh registry
// compiles, starts and broadcasts without error — to nobody, so every
// session_state / sessions_update frame silently stops reaching dashboards.
func TestNewHub_BroadcasterSharesRegistry(t *testing.T) {
	t.Parallel()
	hub := newHubForTest(HubOptions{Router: session.NewRouter(session.RouterConfig{})}, sendEngineOpts{})
	t.Cleanup(hub.Shutdown)
	if hub.bcast == nil {
		t.Fatal("NewHub did not build the broadcaster")
	}
	if hub.bcast.recipients != hub.subs {
		t.Error("bcast.recipients is not hub.subs — broadcasts would go to a registry no client ever joins")
	}
}

// TestSendEngine_NotifyAfterDrainDoesNotArmPending is RFC send-engine-extraction
// §6 test ③: once Shutdown has drained the engine, no notify path may take a
// bcast.pending slot — the debounce arm in BroadcastSessionsUpdate is the one
// that can, and once closed it must decline. A late
// remoteSend goroutine that slipped past drain would otherwise arm a
// broadcast callback that runs after Shutdown emptied the client set.
//
// Observed behaviourally: after Shutdown, every sendNotifier method is called
// through the engine's own handle; the debounce timer must stay disarmed and
// both bcast.wait and clientWG.Wait must return immediately (a stuck Wait
// means an Add with no matching Done was registered post-drain).
func TestSendEngine_NotifyAfterDrainDoesNotArmPending(t *testing.T) {
	t.Parallel()
	hub := newHubForTest(HubOptions{Router: session.NewRouter(session.RouterConfig{})}, sendEngineOpts{})
	hub.Shutdown()

	if _, shuttingDown := hub.engine.TrackSend(); !shuttingDown {
		t.Fatal("TrackSend after Shutdown reported shuttingDown=false")
	}
	n := hub.engine.notify
	n.BroadcastSessionsUpdate()
	n.BroadcastSessionReady("k")
	n.broadcastState("k", "running", "")
	n.broadcastSendError("k", "boom")

	hub.bcast.debounce.mu.Lock()
	armed := hub.bcast.debounce.armed
	hub.bcast.debounce.mu.Unlock()
	if armed {
		t.Error("BroadcastSessionsUpdate armed the debounce timer after Shutdown — a pending slot was taken past drain")
	}

	done := make(chan struct{})
	go func() {
		hub.bcast.wait()
		hub.clientWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bcast.wait / clientWG.Wait did not return after post-Shutdown notify calls — a post-drain Add leaked")
	}
}

// TestBuildServer_SharesOneWSStack pins buildWSStack's one-of-each: the
// dispatcher's engine (wiring), the dashboard's (SendHandler) and the Hub's
// are one instance; it notifies the one broadcaster the Hub and the producers
// hold; and that broadcaster fans out to the Hub's own registry. A second
// broadcaster or registry anywhere compiles and runs — frames just stop
// reaching the clients the Hub admitted.
func TestBuildServer_SharesOneWSStack(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	srv, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	t.Cleanup(srv.appCancel)
	hub, w := srv.hub, hs.wiring

	if w.engine == nil || w.bcast == nil {
		t.Fatal("buildWSStack left wiring.engine / wiring.bcast unset")
	}
	if hub.engine != w.engine || hs.sendH.engine != w.engine {
		t.Error("Hub, SendHandler and wiring do not share one engine")
	}
	if hub.bcast != w.bcast {
		t.Error("the Hub's broadcaster is not wiring.bcast — construction-time producers would broadcast to a different client set")
	}
	if w.engine.notify != sendNotifier(w.bcast) {
		t.Error("engine.notify is not wiring.bcast — send-path session_state / send_error frames would reach a different client set")
	}
	if hub.subs != w.bcast.recipients {
		t.Error("hub.subs is not bcast.recipients — broadcasts would go to a registry no client ever joins")
	}
	if w.engine.router != sendEngineRouter(router) || hub.router != HubRouter(router) {
		t.Error("engine and Hub do not share the Server's router")
	}
	if w.engine.resolver != hub.resolver || w.engine.allowedRoot != hub.tailers.allowedRoot {
		t.Error("engine and Hub do not share the resolver / allowedRoot")
	}
	// The engine's ctx is its own child of appCtx: a SIGTERM (appCancel)
	// still reaches in-flight engine sends without Hub.Shutdown.
	srv.appCancel()
	select {
	case <-w.engine.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling appCtx did not cancel the engine's ctx")
	}
}

// TestSendEngine_DrainCancelsOwnCtx replaces the "engine.ctx == hub.ctx"
// contract: drain cancels the engine's own ctx before it waits, so a tracked
// goroutine blocked on e.ctx (remoteSend's RPC, ownerLoop's collect wait)
// returns at once instead of after its timeout — and the parent is untouched.
func TestSendEngine_DrainCancelsOwnCtx(t *testing.T) {
	t.Parallel()
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	e := newSendEngineForTest(sendEngineOpts{Ctx: parent})

	release, shuttingDown := e.TrackSend()
	if shuttingDown {
		t.Fatal("fresh engine: TrackSend reported shuttingDown")
	}
	go func() {
		defer release()
		<-e.ctx.Done()
	}()
	drained := make(chan struct{})
	go func() {
		e.drain()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return within 2s: it waited on a goroutine blocked on e.ctx without cancelling it")
	}
	if parent.Err() != nil {
		t.Error("drain cancelled the parent ctx; it may cancel only the engine's own")
	}
}

// TestNewHub_RequiresEngine pins NewHub's construction-time refusal: a Hub
// that built its own engine or broadcaster as a fallback would be a second
// send pipeline / client set that buildWSStack's siblings never see.
func TestNewHub_RequiresEngine(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]HubOptions{
		"neither":        {},
		"no engine":      {Broadcaster: newWSBroadcaster(newSubscriberRegistry())},
		"no broadcaster": {Engine: newSendEngineForTest(sendEngineOpts{})},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: NewHub accepted it", name)
				}
			}()
			NewHub(opts)
		}()
	}
}
