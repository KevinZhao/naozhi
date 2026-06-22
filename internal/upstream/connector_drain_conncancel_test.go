package upstream

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/session"
)

// warnCaptureHandler records whether a "drain exceeded budget" Warn was
// emitted by handleConn's drain defer. We only care about the message text.
type warnCaptureHandler struct {
	mu  sync.Mutex
	hit bool
}

func (h *warnCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "drain exceeded budget") {
		h.mu.Lock()
		h.hit = true
		h.mu.Unlock()
	}
	return nil
}
func (h *warnCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCaptureHandler) WithGroup(string) slog.Handler      { return h }
func (h *warnCaptureHandler) drained() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hit
}

// TestHandleConn_ReadErrorCancelsConnCtxBeforeDrain pins #2222: on a plain
// ReadJSON-error disconnect (parent ctx still live), the drain defer must
// cancel connCtx BEFORE wg.Wait() so the ping goroutine — which selects only
// on its 30s ticker and connCtx.Done — exits at once instead of parking for
// the full drain budget and emitting a spurious "drain exceeded budget" WARN.
//
// Determinism: the parent ctx stays live the whole time, so the ONLY thing
// that can release the parked ping goroutine within the budget is connCancel()
// firing first inside the drain defer. We shorten the budget so that, with the
// pre-fix LIFO ordering (connCancel runs AFTER wg.Wait), the budget timer would
// deterministically fire and set the WARN flag. handleConn must instead return
// in well under the budget with no WARN.
func TestHandleConn_ReadErrorCancelsConnCtxBeforeDrain(t *testing.T) {
	// Short budget: with the bug the parked ping goroutine pins wg.Wait until
	// this fires, tripping the WARN. With the fix wg.Wait returns immediately.
	orig := handleConnDrainBudget
	handleConnDrainBudget = 2 * time.Second
	t.Cleanup(func() { handleConnDrainBudget = orig })

	cap := &warnCaptureHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	// Server side: accept the socket then immediately close it so the
	// connector's ReadJSON errors out with the parent ctx still live.
	closed := make(chan struct{})
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		conn.Close()
		close(closed)
	})
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, r, nil, nil)

	// Live parent ctx: never cancelled during the test. This isolates the fix
	// — only connCancel() inside the drain defer can release the ping ticker.
	ctx := context.Background()

	hcDone := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(hcDone)
		_ = c.handleConn(ctx, conn)
	}()

	<-closed

	select {
	case <-hcDone:
	case <-time.After(handleConnDrainBudget + 2*time.Second):
		t.Fatal("handleConn did not return after the socket closed")
	}
	elapsed := time.Since(start)

	if cap.drained() {
		t.Errorf("drain budget WARN was emitted: connCtx was not cancelled before wg.Wait(); "+
			"the ping goroutine parked for the full %v budget (#2222 regression)", handleConnDrainBudget)
	}
	// With the fix the ping goroutine exits on connCtx.Done immediately, so
	// handleConn returns far below the budget. Allow generous scheduler slack.
	if elapsed >= handleConnDrainBudget {
		t.Errorf("handleConn took %v (>= budget %v): drain did not short-circuit (#2222 regression)",
			elapsed, handleConnDrainBudget)
	}
}
