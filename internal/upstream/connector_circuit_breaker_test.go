package upstream

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// alwaysFailServer returns an HTTP server that rejects every WebSocket
// upgrade attempt with 500. runOnce sees this as a dial error, which is
// exactly the "persistently broken primary" scenario the breaker covers.
func alwaysFailServer(t *testing.T) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv.Close
}

// capturingLogger routes slog output into a bytes.Buffer protected by
// a mutex so parallel handlers don't race on the underlying buffer.
type capturingLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturingLogger) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturingLogger) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// runUntil runs c.Run until cond holds, then stops it. Run's first backoff is
// a hard-coded 1s and a breaker backoff below that never lowers it, so each
// attempt costs ~1s (jittered); the 10s bound leaves room for five of them.
func runUntil(t *testing.T, c *Connector, cond func() bool, msg string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	testhelper.Eventually(t, cond, 10*time.Second, msg)
}

// TestConnector_CircuitBreakerTripsAndEmitsSingleWARN drives Run against
// a server that fails every upgrade. With threshold=2, the 2nd consecutive
// failure must emit exactly one "circuit breaker tripped" WARN. Subsequent failures
// stay silent on the breaker axis but keep emitting the per-attempt
// "connector disconnected" WARN.
//
// NOT t.Parallel(): mutates process-wide state — slog.SetDefault plus the
// package-level tunables circuitBreakerThreshold, circuitBreakerBackoff, and
// (TestConnector_CircuitBreakerResetsOnSuccess only) handleConnDrainBudget.
// Running these tests in parallel with anything that also touches those
// would produce races and flaky assertions.
func TestConnector_CircuitBreakerTripsAndEmitsSingleWARN(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: skipped in -short")
	}
	origThreshold := circuitBreakerThreshold
	origBreakerBackoff := circuitBreakerBackoff
	circuitBreakerThreshold = 2
	circuitBreakerBackoff = 50 * time.Millisecond
	t.Cleanup(func() {
		circuitBreakerThreshold = origThreshold
		circuitBreakerBackoff = origBreakerBackoff
	})

	wsAddr, closeSrv := alwaysFailServer(t)
	defer closeSrv()

	origLogger := slog.Default()
	cap := &capturingLogger{}
	slog.SetDefault(slog.New(slog.NewTextHandler(cap, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	cfg := &Config{URL: wsAddr, NodeID: "node-cb", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

	// Wait for a failure after the trip so "exactly once" is tested against
	// a repeat failure in the tripped state.
	runUntil(t, c, func() bool { return strings.Count(cap.String(), "connector disconnected") >= 3 },
		"want at least 3 failed attempts")

	out := cap.String()

	// Must emit the breaker tripped WARN exactly once — repeated WARNs
	// on every failure would defeat the entire noise-reduction point.
	trippedCount := strings.Count(out, "connector circuit breaker tripped")
	if trippedCount != 1 {
		t.Errorf("circuit breaker tripped WARN count = %d, want 1. Log:\n%s", trippedCount, out)
	}

	// Must still emit per-attempt disconnect WARN so operators see each
	// failure reason — the breaker is about backoff noise, not about
	// hiding the actual errors.
	if !strings.Contains(out, "connector disconnected") {
		t.Errorf("expected per-attempt 'connector disconnected' WARN. Log:\n%s", out)
	}
}

// TestConnector_CircuitBreakerResetsOnSuccess verifies that a single
// successful session clears the breaker state, so the *next* failure
// streak has to accumulate threshold failures again before the breaker
// re-trips. Without a reset, a flaky primary would permanently sit in
// breaker mode and eat a 5-minute delay on every hiccup.
//
// NOT t.Parallel(): mutates process-wide state — slog.SetDefault plus the
// package-level tunables circuitBreakerThreshold, circuitBreakerBackoff, and
// (TestConnector_CircuitBreakerResetsOnSuccess only) handleConnDrainBudget.
// Running these tests in parallel with anything that also touches those
// would produce races and flaky assertions.
func TestConnector_CircuitBreakerResetsOnSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: skipped in -short")
	}
	origThreshold := circuitBreakerThreshold
	origBreakerBackoff := circuitBreakerBackoff
	origDrainBudget := handleConnDrainBudget
	circuitBreakerThreshold = 2
	circuitBreakerBackoff = 50 * time.Millisecond
	// Shorten drain so handleConn returns promptly after the server
	// closes the conn, letting Run's next loop iteration hit the
	// "connected" branch quickly.
	handleConnDrainBudget = 200 * time.Millisecond
	t.Cleanup(func() {
		circuitBreakerThreshold = origThreshold
		circuitBreakerBackoff = origBreakerBackoff
		handleConnDrainBudget = origDrainBudget
	})

	// The server alternates: first 2 attempts fail, then one succeeds,
	// then fails again. We drive Run across the transition. Threshold=2
	// means attempt 2's failure trips the breaker; attempt 3's success
	// clears it.
	var attempts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&attempts, 1)
		if n == 3 {
			// Successful WS upgrade + immediate valid registered ack.
			// runOnce returns connected=true as soon as the registered
			// ack is processed; we then close the conn to force a quick
			// post-success loop iteration (the reset branch runs at the
			// top of the *next* loop iteration, not during the success).
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			var reg map[string]any
			if err := conn.ReadJSON(&reg); err == nil {
				_ = conn.WriteJSON(map[string]string{"type": "registered"})
			}
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	wsAddr := "ws" + strings.TrimPrefix(srv.URL, "http")

	origLogger := slog.Default()
	cap := &capturingLogger{}
	slog.SetDefault(slog.New(slog.NewTextHandler(cap, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	cfg := &Config{URL: wsAddr, NodeID: "node-reset", Token: "t"}
	c := New(cfg, testRouter(makeRouter()), nil, Discovery{}, nil)

	// Run until the breaker has tripped a second time after the success.
	runUntil(t, c, func() bool { return strings.Count(cap.String(), "connector circuit breaker tripped") >= 2 },
		"want a second breaker trip after the successful connection")

	out := cap.String()

	// Must have tripped at least once (during the first 2 failures).
	trippedCount := strings.Count(out, "connector circuit breaker tripped")
	if trippedCount < 1 {
		t.Errorf("expected breaker to trip at least once before the success, got %d. Log:\n%s", trippedCount, out)
	}

	// Must have emitted a reset INFO after the success.
	resetCount := strings.Count(out, "connector circuit breaker reset")
	if resetCount < 1 {
		t.Errorf("expected 'circuit breaker reset' after successful connection, got %d. Log:\n%s", resetCount, out)
	}

	// The counter reset itself, not just its log line: the log above is
	// emitted by a different statement than `consecutiveFailures = 0`. With
	// the counter reset, the second trip needs `threshold` fresh failures and
	// its WARN reports exactly that; without it the count keeps climbing.
	if n := strings.Count(out, "consecutive_failures="+strconv.Itoa(circuitBreakerThreshold)); n < 2 {
		t.Errorf("consecutive_failures=%d reported %d times, want 2; the failure counter was not reset by the successful connection. Log:\n%s",
			circuitBreakerThreshold, n, out)
	}
}
