package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/leakcheck"
)

// TestWSRelay_NoGoroutineLeakAfterClose pins R247-ARCH-22 (#679) for
// the wsRelay shutdown path: after a Close that successfully drains
// the wsRelay.wg, the process must not be carrying any extra
// goroutines started by the relay layer.
//
// The existing relay_waitgroup_test.go suite asserts the WG accounting
// is correct (Close blocks while sendHistoryToClient is parked). What
// it cannot assert is "no goroutine started outside the WG scope is
// still alive" — a refactor that adds a worker goroutine but forgets
// to wg.Add for it would pass the existing tests and silently leak in
// production.
//
// leakcheck.Check is the missing piece: it captures a baseline at the
// top of the test and fails if the count grows beyond a small grace
// window (DefaultGrace=2) by the time the deferred closure runs.
func TestWSRelay_NoGoroutineLeakAfterClose(t *testing.T) {
	defer leakcheck.Check(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if !authHandshake(t, conn) {
				return
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				var msg ClientMsg
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
			}
		case "/api/sessions/events":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	first := &mockSink{id: 1}
	relay.Subscribe(first, "k", 0)
	// Allow the first-subscriber path to settle so we are not
	// measuring the in-flight goroutine count.
	time.Sleep(80 * time.Millisecond)

	relay.Close()
	// The deferred leakcheck.Check now waits up to DefaultSettleWindow
	// for the count to fall back to the captured baseline + grace; if
	// any goroutine started by the relay is still parked at that point
	// it is reported as a leak with a full stack dump.
}
