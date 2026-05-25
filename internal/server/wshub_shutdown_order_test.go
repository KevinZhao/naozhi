package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	var nodesMu sync.RWMutex
	hub := NewHub(HubOptions{
		Router:  router,
		Guard:   guard,
		NodesMu: &nodesMu,
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
