package upstream

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
)

// TestCircuitBreakerVars_PackageLevelVars locks the ARCH-D6 (Round 177)
// circuit breaker knobs into package-level vars so tests can shorten
// values without wall-clock waits. A future refactor turning them into
// const would silently break the breaker regression tests — they'd
// either wait full 5 minutes per failure (slow CI) or skip coverage.
func TestCircuitBreakerVars_PackageLevelVars(t *testing.T) {
	src, err := os.ReadFile("connector.go")
	if err != nil {
		t.Fatalf("read connector.go: %v", err)
	}
	thresholdVar := regexp.MustCompile(`var\s+circuitBreakerThreshold\s*=\s*`)
	if !thresholdVar.Match(src) {
		t.Error("circuitBreakerThreshold must be a package-level var for test injection")
	}
	backoffVar := regexp.MustCompile(`var\s+circuitBreakerBackoff\s*=\s*`)
	if !backoffVar.Match(src) {
		t.Error("circuitBreakerBackoff must be a package-level var for test injection")
	}
	if circuitBreakerThreshold <= 0 {
		t.Errorf("circuitBreakerThreshold = %d, want > 0", circuitBreakerThreshold)
	}
	if circuitBreakerBackoff <= 0 {
		t.Errorf("circuitBreakerBackoff = %v, want > 0", circuitBreakerBackoff)
	}
	// Breaker backoff must exceed the 30 s ceiling — otherwise tripping
	// it wouldn't change behaviour in a meaningful way.
	if circuitBreakerBackoff <= 30*time.Second {
		t.Errorf("circuitBreakerBackoff = %v must exceed the 30 s normal ceiling", circuitBreakerBackoff)
	}
}

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

// TestConnector_CircuitBreakerTripsAndEmitsSingleWARN drives Run against
// a server that fails every upgrade. With threshold=2 and breaker
// backoff=50ms, the 3rd loop iteration (2 consecutive failures) must
// emit exactly one "circuit breaker tripped" WARN. Subsequent failures
// stay silent on the breaker axis but keep emitting the per-attempt
// "connector disconnected" WARN.
//
// NOT t.Parallel(): mutates process-wide state — slog.SetDefault plus the
// package-level tunables circuitBreakerThreshold, circuitBreakerBackoff, and
// (TestConnector_CircuitBreakerResetsOnSuccess only) handleConnDrainBudget.
// Running these tests in parallel with anything that also touches those
// would produce races and flaky assertions.
func TestConnector_CircuitBreakerTripsAndEmitsSingleWARN(t *testing.T) {
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

	cfg := &config.UpstreamConfig{URL: wsAddr, NodeID: "node-cb", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	// Run for long enough to let 3+ failures accumulate (each attempt
	// is fast because the server rejects immediately; sleeps are
	// bounded by 50 ms breaker backoff).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	<-done

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

	cfg := &config.UpstreamConfig{URL: wsAddr, NodeID: "node-reset", Token: "t"}
	c := New(cfg, makeRouter(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	<-done

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
}

// TestRun_BreakerSourceContract locks the control flow in Run so future
// refactors of the reconnect loop cannot silently remove the breaker
// logic. We scan connector.go for the key structural anchors:
//   - consecutiveFailures variable declared in Run
//   - circuitTripped flag
//   - reset-on-connected branch
//   - trip-on-threshold branch with single-shot WARN gate
//   - backoff floor raised to circuitBreakerBackoff
//
// Unlike a behavioural test, this catches "someone removed the !connected
// branch by accident" even if the behavioural tests above happen to
// still pass (e.g. timing flukes).
func TestRun_BreakerSourceContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("connector.go")
	if err != nil {
		t.Fatalf("read connector.go: %v", err)
	}
	body := string(src)
	// Extract the Run function body — rough but good enough for this
	// structural check.
	runIdx := strings.Index(body, "func (c *Connector) Run(ctx context.Context) {")
	if runIdx < 0 {
		t.Fatal("Run method not found in connector.go")
	}
	// End marker: the next top-level func declaration.
	rest := body[runIdx:]
	endIdx := strings.Index(rest[1:], "\nfunc ")
	if endIdx < 0 {
		endIdx = len(rest)
	} else {
		endIdx++ // account for the leading \n offset
	}
	runBody := rest[:endIdx]

	anchors := []string{
		"consecutiveFailures",
		"circuitTripped",
		"if connected {",
		"consecutiveFailures++",
		"circuitBreakerThreshold",
		"circuitBreakerBackoff",
		"connector circuit breaker tripped",
	}
	for _, anchor := range anchors {
		if !strings.Contains(runBody, anchor) {
			t.Errorf("Run body missing breaker anchor %q — circuit breaker may have been regressed out", anchor)
		}
	}

	// Single-shot WARN gate: the "tripped" WARN must be inside a
	// `if !circuitTripped` guard (or equivalent shape) so repeat
	// failures don't flood logs. We check for the textual
	// composition rather than the exact pattern.
	gateIdx := strings.Index(runBody, "if !circuitTripped {")
	warnIdx := strings.Index(runBody, "connector circuit breaker tripped")
	if gateIdx < 0 || warnIdx < 0 || gateIdx > warnIdx {
		t.Errorf("single-shot WARN gate missing: expect `if !circuitTripped {` to precede the tripped WARN line")
	}
}
