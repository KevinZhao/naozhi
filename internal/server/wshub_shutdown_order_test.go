package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// TestHubShutdown_WiredLinkersNiledAfterClientWGWait locks issue #371: the
// nil-out of h.wiredLinkers must run AFTER h.clientWG.Wait(). If the order
// is reversed, an in-flight readPump goroutine that calls
// maybeWireLinkerTailer between the nil-out and the Wait would observe
// wiredLinkers == nil and silently take the "Hub shutting down — skip"
// branch (wshub_agent.go:81), dropping a wiring it should have completed.
//
// Test strategy: register a fake clientWG slot owned by the test and start
// Shutdown in a goroutine. Shutdown will block at clientWG.Wait() because
// our slot is still outstanding. We then poll wiredLinkers under its
// mutex — under the FIXED ordering it stays non-nil during the entire
// Wait window; under the BUGGY ordering (nil before Wait) the map would
// already be nil before we get here. After we Done() the slot Shutdown
// completes and we verify the nil-out happened post-Wait.
//
// The polling window is bounded by a deadline that is large enough to
// dwarf goroutine-scheduling jitter (up to 200ms) but short enough to
// fail the suite quickly on regression. -race is the primary detector
// for the surrounding shutdown serialisation; this test specifically
// targets the ORDER between Wait and the nil-out, which is a
// happens-before property race-detector cannot catch on its own.
func TestHubShutdown_WiredLinkersNiledAfterClientWGWait(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	hub := NewHub(HubOptions{
		Router: router,
		Guard:  guard,
	})

	hub.wiredLinkersMu.Lock()
	preInit := hub.wiredLinkers != nil
	hub.wiredLinkersMu.Unlock()
	if !preInit {
		t.Fatal("wiredLinkers unexpectedly nil before Shutdown — NewHub contract changed")
	}

	hub.clientWG.Add(1)

	shutdownDone := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(shutdownDone)
	}()

	var observedNilDuringWait atomic.Bool
	pollDeadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(pollDeadline) {
		hub.wiredLinkersMu.Lock()
		isNil := hub.wiredLinkers == nil
		hub.wiredLinkersMu.Unlock()
		if isNil {
			observedNilDuringWait.Store(true)
			break
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before our clientWG slot was released — clientWG.Wait did not actually wait")
	default:
	}

	hub.clientWG.Done()

	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown hung past 5s after clientWG slot release")
	}

	if observedNilDuringWait.Load() {
		t.Error("wiredLinkers was niled while Shutdown was still inside clientWG.Wait — issue #371 regressed: an in-flight readPump reaching maybeWireLinkerTailer here would silently skip wiring")
	}

	hub.wiredLinkersMu.Lock()
	post := hub.wiredLinkers
	hub.wiredLinkersMu.Unlock()
	if post != nil {
		t.Error("wiredLinkers not niled after Shutdown — GC-leak fix regressed")
	}
}

// TestHubShutdown_OrderingInSource is a source-level guardrail complementing
// the behavioural test: it scans wshub.go and asserts that the
// h.clientWG.Wait() line appears BEFORE h.wiredLinkers = nil inside
// Shutdown. Issue #371.
func TestHubShutdown_OrderingInSource(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(self), "wshub.go")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	body := string(raw)

	waitIdx := strings.Index(body, "h.clientWG.Wait()")
	if waitIdx < 0 {
		t.Fatal("wshub.go: no h.clientWG.Wait() call found — Shutdown contract changed")
	}
	nilIdx := strings.Index(body, "h.wiredLinkers = nil")
	if nilIdx < 0 {
		t.Fatal("wshub.go: no h.wiredLinkers = nil assignment found — Shutdown contract changed")
	}
	if waitIdx >= nilIdx {
		t.Errorf("wshub.go: h.clientWG.Wait() (offset %d) must appear BEFORE h.wiredLinkers = nil (offset %d) — issue #371 regressed", waitIdx, nilIdx)
	}
}

// TestHubShutdown_SendDrainPositionInSource pins where h.engine.drain() sits
// inside Shutdown (#2551). The barrier used to be four inline statements
// (sendTrackMu / sendClosed / sendWG.Wait); collapsing it into one call makes
// it look movable, and it is not: the goroutines drain waits on re-enter
// h.mu / authMu / debounceMu through sendNotifier (BroadcastSessionReady →
// authMu.RLock, broadcastState → h.mu.RLock, BroadcastSessionsUpdate →
// debounceMu.Lock). Tidying the call up into the debounceMu critical section —
// which is where the "close the window" statements live and therefore the most
// tempting home for it — deadlocks deterministically: the in-flight goroutine
// is already past the debounceClosedFast fast path and blocked on
// debounceMu.Lock while Shutdown holds it waiting for wg.
//
// -race cannot catch this (a lock-order inversion against a WaitGroup is not a
// data race) and the behavioural tests cannot either — they would simply hang
// until the suite's own timeout. Hence a source-order assertion:
//
//	h.cancel()  <  debounceMu.Unlock()  <  h.clientWG.Wait()  <  drain()  <  nodes Close
func TestHubShutdown_SendDrainPositionInSource(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "wshub.go"))
	if err != nil {
		t.Fatalf("read wshub.go: %v", err)
	}
	body := string(raw)

	shutdownIdx := strings.Index(body, "func (h *Hub) Shutdown() {")
	if shutdownIdx < 0 {
		t.Fatal("wshub.go: Hub.Shutdown not found")
	}
	// Scope every offset to Shutdown's body so an identical call elsewhere in
	// the file cannot satisfy the ordering by accident.
	sd := body[shutdownIdx:]

	// Ordered low → high, each with the reason a violation breaks something.
	steps := []struct{ marker, why string }{
		{"h.cancel()", "without ctx cancelled first, drain blocks for the full remote-RPC timeout instead of returning promptly"},
		{"h.debounceMu.Unlock()", "drain must not hold debounceMu — the drained goroutines take it via BroadcastSessionsUpdate"},
		{"h.clientWG.Wait()", "client goroutines settle first; drain is the send-side barrier"},
		{"h.engine.drain()", "the send barrier sits here; see sendEngine.drain's CALL-SITE PRECONDITIONS"},
		{"h.nodes.Conns()", "nodes must close AFTER drain, or an in-flight remote RPC writes to a closed nc.conn"},
	}
	prev, prevMarker := -1, ""
	for _, s := range steps {
		idx := strings.Index(sd, s.marker)
		if idx < 0 {
			t.Fatalf("wshub.go Shutdown: %q not found — the send-drain ordering contract cannot be checked (#2551)", s.marker)
		}
		if idx <= prev {
			t.Errorf("wshub.go Shutdown: %q (offset %d) must come after %q (offset %d) — %s",
				s.marker, idx, prevMarker, prev, s.why)
		}
		prev, prevMarker = idx, s.marker
	}
}
