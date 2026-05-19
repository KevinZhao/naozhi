package upstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// handleConnDrainBudget is the hard deadline applied to the deferred
// wg.Wait() at the end of handleConn. Every worker goroutine is expected
// to honour connCtx, which is cancelled the moment handleConn returns; the
// budget covers the pathological case where a stuck downstream call (most
// notably sess.Send blocked on CLI watchdog timeout ≈ 5 min) refuses to
// unblock. Hit-budget leaks the stuck goroutine to process teardown —
// strictly better than pinning the whole upstream reconnect loop on one
// slow session. R51-REL-005. Package-level var (not const) so tests can
// shorten it without wall-clock waits.
var handleConnDrainBudget = 15 * time.Second

// circuitBreakerThreshold is the number of consecutive runOnce failures
// that triggers the circuit breaker. With the 1s→30s backoff schedule
// (doubling: 1, 2, 4, 8, 16, 30), 6 failures cover ≈ 1 minute of wall
// time before the longer breaker backoff kicks in.
//
// ARCH-D6 (Round 177): prior to this, a mis-configured primary (wrong URL,
// wrong token, network partition) would reconnect forever at a fixed 30s
// ceiling — a steady log firehose and constant CPU on both sides with no
// signal that "this has been failing for a while now". The breaker trades
// a longer-but-finite recovery delay for a single sharp WARN when things
// are clearly broken.
var circuitBreakerThreshold = 6

// circuitBreakerBackoff is the backoff floor applied once the breaker
// trips. 5 minutes is short enough that transient outages (DNS hiccup,
// primary restart, cert rollover) still auto-recover without operator
// intervention, but long enough to cut log noise dramatically versus the
// 30s ceiling.
var circuitBreakerBackoff = 5 * time.Minute

// reasonSessionReset is the Reason value emitted for the terminal
// session_state message in streamEvents when the router has already dropped
// the session (Reset raced ahead of the notify-close path). Centralised so
// downstream consumers (reverseconn.go, dashboard.js) have one literal to
// match on, not a scatter of stringly-typed tokens. RNEW-005.
const reasonSessionReset = "session_reset"

// Connector dials a primary naozhi and serves it as a reverse-connected node.
// Run on machines behind NAT that cannot be reached by the primary directly.
type Connector struct {
	cfg *config.UpstreamConfig
	// router is the SessionRouter subset used by Connector (consumer.go).
	// *session.Router satisfies this interface implicitly. Kept as an
	// interface so future Router sub-aggregation and connector tests
	// can swap implementations without touching upstream internals.
	router  SessionRouter
	projMgr *project.Manager // may be nil
	// resolver centralises planner-view opts derivation for
	// reverse-RPC restart_planner (#7). Nil keeps the legacy literal
	// AgentOpts construction for headless/test callers that don't wire
	// a resolver. docs/rfc/key-resolver.md Phase 5.
	resolver         *session.KeyResolver
	claudeDir        string
	hostname         string
	defaultWorkspace string // used as allowedRoot for incoming workspace overrides
	discoverFunc     func() (json.RawMessage, error)
	previewFunc      func(sessionID string) (json.RawMessage, error)
}

// New creates a Connector. projMgr may be nil if projects are not configured.
// Callers that want the KeyResolver-backed planner restart path should
// pass a non-nil resolver (built from session.NewKeyResolver +
// project.NewDataSource). Nil resolver keeps the legacy inlined merge
// for backward compatibility with existing tests.
func New(cfg *config.UpstreamConfig, router *session.Router, projMgr *project.Manager, resolver *session.KeyResolver) *Connector {
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	hostname, err := os.Hostname()
	if err != nil {
		slog.Warn("upstream: os.Hostname failed; using 'unknown' identity", "err", err)
		hostname = "unknown"
	}
	return &Connector{
		cfg:              cfg,
		router:           router,
		projMgr:          projMgr,
		resolver:         resolver,
		claudeDir:        claudeDir,
		hostname:         hostname,
		defaultWorkspace: router.DefaultWorkspace(),
	}
}

// SetDiscoverFunc sets a callback that returns discovered sessions as JSON.
func (c *Connector) SetDiscoverFunc(fn func() (json.RawMessage, error)) {
	c.discoverFunc = fn
}

// SetPreviewFunc sets a callback that returns conversation history for a discovered session.
func (c *Connector) SetPreviewFunc(fn func(sessionID string) (json.RawMessage, error)) {
	c.previewFunc = fn
}

// Run connects to the primary and serves requests. Reconnects on disconnect.
// Blocks until ctx is cancelled.
//
// Reconnect schedule: 1s → 2s → 4s → 8s → 16s → 30s (ceiling), all jittered
// in [0.75x, 1.25x). Any successful session resets backoff to 1s.
//
// ARCH-D6 (Round 177) circuit breaker: once runOnce fails consecutively
// circuitBreakerThreshold times with no intervening success, the backoff
// floor jumps to circuitBreakerBackoff (5 min) and a single breaker-tripped
// WARN is emitted. Subsequent failures stay at the 5 min floor with no
// repeated WARN — this cuts log noise on mis-configured primaries from
// every ~30s to every 5 min while still auto-recovering on the first
// success. The per-attempt "connector disconnected" WARN continues to
// fire so operators can still see each failure reason.
func (c *Connector) Run(ctx context.Context) {
	backoff := time.Second
	consecutiveFailures := 0
	circuitTripped := false
	for {
		connected, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("connector disconnected", "url", c.cfg.URL, "err", err)
		}
		// Track consecutive failures for the circuit breaker. A
		// "successful session" here means we connected and stayed up
		// long enough that runOnce returned connected=true, even if the
		// eventual disconnect surfaced an error.
		if connected {
			consecutiveFailures = 0
			if circuitTripped {
				slog.Info("connector circuit breaker reset after successful connection", "url", c.cfg.URL)
				circuitTripped = false
			}
			// Reset backoff after a successful session so reconnect
			// after sleep/restart is fast (1s) rather than up to 30s.
			backoff = time.Second
		} else {
			consecutiveFailures++
			if consecutiveFailures >= circuitBreakerThreshold {
				if !circuitTripped {
					slog.Warn("connector circuit breaker tripped, extending backoff",
						"url", c.cfg.URL,
						"consecutive_failures", consecutiveFailures,
						"backoff", circuitBreakerBackoff)
					circuitTripped = true
				}
				if backoff < circuitBreakerBackoff {
					backoff = circuitBreakerBackoff
				}
			}
		}
		// Jitter the sleep so many connectors restarted together (e.g. fleet
		// SIGHUP) don't hammer the primary on aligned deadlines. backoff
		// still doubles deterministically; we only scatter wall-time.
		timer := time.NewTimer(jitterBackoff(backoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// Only double within the normal 1s→30s ceiling. Once the
			// breaker has tripped, backoff stays pinned at
			// circuitBreakerBackoff until the next successful connect
			// clears it.
			if backoff < circuitBreakerBackoff {
				backoff = min(backoff*2, 30*time.Second)
			}
		}
	}
}

func (c *Connector) runOnce(ctx context.Context) (bool, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// Pin TLS floor so downgraded clients can't be forced onto a weaker
		// protocol via a compromised network segment. wss:// is already
		// required by config validation.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	conn, _, dialErr := dialer.DialContext(ctx, c.cfg.URL, nil)
	if dialErr != nil {
		return false, fmt.Errorf("dial: %w", dialErr)
	}
	// R188-SEC-L2: surface operator signal when tokens are transmitted
	// over plaintext ws:// (requires config.upstream.insecure=true to pass
	// validation). A single warn per successful dial is enough for ops
	// dashboards to catch forgotten insecure mode without spamming the
	// journal on reconnect loops.
	if strings.HasPrefix(c.cfg.URL, "ws://") {
		slog.Warn("upstream connector: transmitting token over plaintext ws:// — set upstream.insecure=false and use wss:// for production")
	}
	// Bound inbound frame size so a malicious or buggy primary cannot
	// exhaust memory with a single huge message. 16 MB matches the primary
	// side's ReverseConn limit (reverseserver.go).
	conn.SetReadLimit(16 << 20)

	// gorilla/websocket's Conn.Close is documented for one concurrent
	// reader and one concurrent writer but not for concurrent Close calls.
	// The cancel-watchdog goroutine below calls conn.Close on ctx.Done, and
	// the deferred close on function exit would race with it. Serialize
	// both paths through a sync.Once so exactly one Close ever fires.
	// R60-GO-M5.
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()

	// Close the WebSocket when ctx is cancelled to unblock ReadJSON in handleConn.
	connDone := make(chan struct{})
	defer close(connDone)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-connDone:
		}
	}()

	// Register. Sprint 6b of the multi-backend RFC: auto-derive
	// Capabilities from the locally-registered backend.Profile set so
	// the primary's selectNodeForBackend can answer "does this node
	// host kiro?" without operator-supplied YAML caps. Each enabled
	// profile contributes its RequiredNodeCaps; we union and sort for
	// deterministic on-the-wire output (eases packet capture review
	// and primary-side log diffing).
	reg := node.ReverseMsg{
		Type:         "register",
		NodeID:       c.cfg.NodeID,
		Token:        c.cfg.Token,
		DisplayName:  c.cfg.DisplayName,
		Hostname:     c.hostname,
		Capabilities: derivedCaps(),
	}
	if err := conn.WriteJSON(reg); err != nil {
		return false, fmt.Errorf("register write: %w", err)
	}

	// SetReadDeadline error means the underlying net.Conn is already torn
	// down — returning early is correct because ReadJSON below would block
	// forever without a deadline. The same applies to the clear below and
	// the pong-path deadlines downstream.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return false, fmt.Errorf("set register read deadline: %w", err)
	}
	var ack node.ReverseMsg
	if err := conn.ReadJSON(&ack); err != nil {
		return false, fmt.Errorf("register ack read: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return false, fmt.Errorf("clear register read deadline: %w", err)
	}

	if ack.Type != "registered" {
		// %q so primary-controlled Error string can't inject key=val pairs or
		// newlines into slog output downstream.
		return false, fmt.Errorf("register failed: %q", ack.Error)
	}
	slog.Info("connected to primary", "url", c.cfg.URL, "node_id", c.cfg.NodeID)

	// Enable WebSocket-level ping/pong for dead connection detection.
	// ReadDeadline resets on any pong response from the primary.
	const wsReadTimeout = 90 * time.Second
	conn.SetPongHandler(func(string) error {
		// SetReadDeadline error here means the conn was torn down between
		// the pong arrival and our refresh; surface it so the outer
		// ReadJSON loop exits via its error path instead of blocking.
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		return false, fmt.Errorf("set initial read deadline: %w", err)
	}

	return true, c.handleConn(ctx, conn)
}

// pingOnce runs a single WebSocket-level ping under writeMu and closes the
// conn on any failure. Returns true if the ping succeeded (caller keeps the
// ticker running), false if the conn was torn down (caller returns).
// Extracted from the ping-loop body so defer writeMu.Unlock() covers every
// exit path — the inline form had three separate manual Unlock sites that
// were easy to miss when adding a new failure branch.
func pingOnce(conn *websocket.Conn, writeMu *sync.Mutex) bool {
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return false
	}
	if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
		_ = conn.Close()
		return false
	}
	return true
}

func marshalResult(v any) (json.RawMessage, error) {
	return json.Marshal(v)
}
