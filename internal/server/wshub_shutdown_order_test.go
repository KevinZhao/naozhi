package server

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

func linkersReleased(h *Hub) bool {
	h.tailers.linkersMu.Lock()
	defer h.tailers.linkersMu.Unlock()
	return h.tailers.linkers == nil
}

// TestHubShutdown_WiredLinkersNiledAfterClientWGWait: the wired-linker set is
// released only after clientWG.Wait. Released earlier, an in-flight readPump
// calling maybeWireLinkerTailer between the release and the Wait would find
// the set gone and silently skip a wiring it should complete.
//
// The test holds a clientWG slot, starts Shutdown and polls the set while
// Shutdown is parked in Wait: it must stay in place for the whole window, and
// be released once the slot is given back.
func TestHubShutdown_WiredLinkersNiledAfterClientWGWait(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	hub := newHubForTest(t, HubOptions{Router: router}, sendEngineOpts{})

	if linkersReleased(hub) {
		t.Fatal("wired linkers released before Shutdown — NewHub contract changed")
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
		if linkersReleased(hub) {
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
		t.Error("wired linkers released while Shutdown was still inside clientWG.Wait: an in-flight readPump reaching maybeWireLinkerTailer here would silently skip wiring")
	}

	if !linkersReleased(hub) {
		t.Error("wired linkers not released after Shutdown — GC-leak fix regressed")
	}
}
