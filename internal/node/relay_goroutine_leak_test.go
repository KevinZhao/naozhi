package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
// The test captures runtime.NumGoroutine at the top and fails if the
// count is still above baseline plus a small grace (2: an HTTP idle-conn
// reaper plus a cleanup helper) after a bounded settle window.
func TestWSRelay_NoGoroutineLeakAfterClose(t *testing.T) {
	baseline := runtime.NumGoroutine()
	defer func() {
		deadline := time.Now().Add(250 * time.Millisecond)
		for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > baseline+2 {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Errorf("goroutine leak: %d goroutines, baseline %d (+2 grace)\n%s", n, baseline, buf)
		}
	}()

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
	relay.Subscribe(first, "k", 0, 0)
	// Allow the first-subscriber path to settle so we are not
	// measuring the in-flight goroutine count.
	time.Sleep(80 * time.Millisecond)

	relay.Close()
	// The deferred check now waits for the count to fall back to the
	// captured baseline + grace; any goroutine started by the relay that
	// is still parked at that point is reported with a full stack dump.
}
