package server

import (
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
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
// passed at all, or passed as a typed-nil *Hub (which reads non-nil through the
// interface). Either one panics on the first broadcast — inside the owner
// goroutine, where ownerLoop's recover turns it into a silently dropped message
// plus one log line.
func TestSendEngine_NotifyNeverNil(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]sendEngineOpts{
		"absent":    {},
		"typed-nil": {Notify: (*Hub)(nil)},
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
// hub_shared_state_test.go's nodes-registry assertion. The engine keeps its own
// reference to each shared dependency instead of a *Hub back-pointer (RFC
// §2.1), which is only safe while both sides point at the SAME instance —
// otherwise a value the Hub path observes and one the send path observes can
// drift, and the bug shows up as "the send used the wrong workspace/profile".
func TestNewHub_SharesDependenciesWithEngine(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	resolver := &session.KeyResolver{}
	agents := map[string]session.AgentOpts{"a": {}}
	hub := NewHub(HubOptions{
		Router:      router,
		Guard:       guard,
		Resolver:    resolver,
		Agents:      agents,
		AllowedRoot: "/tmp/nz-root",
	})
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
	if hub.engine.allowedRoot != hub.allowedRoot {
		t.Errorf("engine.allowedRoot = %q, Hub.allowedRoot = %q — a workspace validated on one path would not be on the other",
			hub.engine.allowedRoot, hub.allowedRoot)
	}
	if hub.engine.notify != sendNotifier(hub) {
		t.Error("engine.notify is not the Hub that built it")
	}
	// The send-block fields must be gone from Hub: the whole point of #2551.
	// Enforced structurally by the build (they no longer exist), so this only
	// documents the intent for the next reader.
	if got := hub.LegacySendInvokes(); got != 0 {
		t.Errorf("fresh Hub LegacySendInvokes = %d, want 0", got)
	}
}
