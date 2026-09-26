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
	guard := session.NewGuard()
	hub := NewHub(HubOptions{
		Router: router,
		Guard:  guard,
	})

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

// TestHubShutdown_SendDrainPositionInSource pins where h.engine.drain() sits
// inside Shutdown (#2551). The barrier used to be four inline statements
// (sendTrackMu / sendClosed / sendWG.Wait); collapsing it into one call makes
// it look movable, and it is not: the goroutines drain waits on re-enter the
// Hub through sendNotifier (BroadcastSessionReady, broadcastState,
// BroadcastSessionsUpdate), so drain has to run after the debouncer is closed
// (a late BroadcastSessionsUpdate then declines instead of taking a clientWG
// slot) and before the nodes close.
//
// -race cannot catch a misplaced barrier (an ordering against a WaitGroup is
// not a data race) and the behavioural tests cannot either — they would simply
// hang until the suite's own timeout. Hence a source-order assertion:
//
//	h.cancel()  <  h.debounce.close()  <  h.clientWG.Wait()  <  drain()  <  nodes Close
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
	// the file cannot satisfy the ordering by accident. Cut at the next
	// top-level func, not at EOF: Shutdown happens to be the last function in
	// wshub.go today, and a helper appended below it would otherwise widen the
	// window silently (#2637).
	sd := body[shutdownIdx:]
	if next := strings.Index(sd[1:], "\nfunc "); next >= 0 {
		sd = sd[:next+1]
	}

	// Ordered low → high, each with the reason a violation breaks something.
	steps := []struct{ marker, why string }{
		{"h.cancel()", "without ctx cancelled first, drain blocks for the full remote-RPC timeout instead of returning promptly"},
		{"h.debounce.close()", "the debouncer closes first, so a drained goroutine's BroadcastSessionsUpdate declines instead of taking a clientWG slot past the Wait"},
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
