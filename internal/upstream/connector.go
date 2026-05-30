package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
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

// reconnectBackoffCeiling is the upper bound for the doubling
// reconnect backoff (1s → 2s → 4s → 8s → 16s → 30s, all jittered).
// Was a hard-coded 30*time.Second literal at the doubling site, which
// made the relationship between this ceiling and circuitBreakerBackoff
// invisible at a glance — the breaker is what kicks in *past* this
// ceiling, so they need to stay readable as a pair. R230C-CR-cleanup
// adjacency: extracted as a const to anchor the godoc that
// circuitBreakerBackoff references ("the 30s ceiling"). Pinned as a
// package-level var (not const) so existing tests can shorten the
// ceiling in the same idiom that handleConnDrainBudget /
// circuitBreakerBackoff already use without wall-clock waits.
var reconnectBackoffCeiling = 30 * time.Second

// circuitBreakerBackoff is the backoff floor applied once the breaker
// trips. 5 minutes is short enough that transient outages (DNS hiccup,
// primary restart, cert rollover) still auto-recover without operator
// intervention, but long enough to cut log noise dramatically versus the
// reconnectBackoffCeiling (30s) ceiling.
//
// NEEDS-DESIGN (R246-ARCH-4): handleConnDrainBudget /
// circuitBreakerThreshold / circuitBreakerBackoff are package-level
// `var` only so existing tests can shorten them without wall-clock
// waits, but the drift cost is that production code path reads
// global mutable state. Plan: migrate all three into UpstreamConfig
// fields (default values via config defaults pass), and expose a
// testutil.WithUpstreamThresholds(t, ...) shim that swaps in a
// reduced-budget *Config for the test-local Connector. Deferred
// until UpstreamConfig overhaul (separate RFC) — flipping these
// now requires touching every test that depends on the package-
// level shortcut, plus a rebuild of the connector's New() signature.
var circuitBreakerBackoff = 5 * time.Minute

// reasonSessionReset is the Reason value emitted for the terminal
// session_state message in streamEvents when the router has already dropped
// the session (Reset raced ahead of the notify-close path). Centralised so
// downstream consumers (reverseconn.go, dashboard.js) have one literal to
// match on, not a scatter of stringly-typed tokens. RNEW-005.
const reasonSessionReset = "session_reset"

// discoverFn is the signature stored behind Connector.discoverFunc.
// Aliased to a named type so atomic.Pointer can box it (Go does not
// allow `atomic.Pointer[func(...)]` without an intermediate name) and
// to give SetDiscoverFunc / loadDiscoverFunc a single source of truth
// for the contract.
type discoverFn func() (json.RawMessage, error)

// previewFn is the signature stored behind Connector.previewFunc.
// Same atomic.Pointer rationale as discoverFn above.
type previewFn func(sessionID string) (json.RawMessage, error)

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
	// R246-ARCH-6: discoverFunc / previewFunc were plain function fields
	// previously, with the wiring contract that "main.go is single-
	// threaded so no reader/writer race exists". Storing under
	// atomic.Pointer instead removes the load-bearing comment and lets
	// the race detector enforce the invariant — any future caller that
	// resets the hook from a goroutine post-Run is now well-defined
	// instead of UB. Read path is loadDiscoverFunc / loadPreviewFunc;
	// nil-safe because atomic.Pointer.Load returns the zero value (nil
	// *T) when never stored.
	discoverFunc atomic.Pointer[discoverFn]
	previewFunc  atomic.Pointer[previewFn]
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
//
// Originally this was a plain field write with the wiring contract that
// "main.go startup is single-threaded so no reader/writer race exists";
// R246-ARCH-6 promoted the field to atomic.Pointer so the race detector
// enforces the invariant rather than relying on a load-bearing comment.
// Concurrent SetDiscoverFunc / handleRequest sequences are now well-
// defined: the most recent Store wins, the read path always sees a
// fully-published function value (or nil if never set).
//
// Calling with a nil fn clears the callback (loadDiscoverFunc returns
// nil and the RPC fast-path returns an empty array). Tests that rebuild
// a Connector per case are unaffected. R233B-ARCH-9 archive anchor:
// when this hook eventually moves out of upstream into a server-
// supplied handler map, drop both setters and let the constructor
// accept the funcs directly so the wiring contract is enforced by the
// type system.
func (c *Connector) SetDiscoverFunc(fn func() (json.RawMessage, error)) {
	if fn == nil {
		c.discoverFunc.Store(nil)
		return
	}
	boxed := discoverFn(fn)
	c.discoverFunc.Store(&boxed)
}

// SetPreviewFunc sets a callback that returns conversation history for a discovered session.
//
// Same atomic.Pointer wiring shape as SetDiscoverFunc — see SetDiscoverFunc
// godoc. Calling with nil clears the callback.
func (c *Connector) SetPreviewFunc(fn func(sessionID string) (json.RawMessage, error)) {
	if fn == nil {
		c.previewFunc.Store(nil)
		return
	}
	boxed := previewFn(fn)
	c.previewFunc.Store(&boxed)
}

// loadDiscoverFunc returns the current discover callback, or nil if
// none was installed. Read path is lock-free; pair with SetDiscoverFunc
// for race-free wiring.
func (c *Connector) loadDiscoverFunc() discoverFn {
	if p := c.discoverFunc.Load(); p != nil {
		return *p
	}
	return nil
}

// loadPreviewFunc returns the current preview callback, or nil if
// none was installed. Read path is lock-free; pair with SetPreviewFunc
// for race-free wiring.
func (c *Connector) loadPreviewFunc() previewFn {
	if p := c.previewFunc.Load(); p != nil {
		return *p
	}
	return nil
}

// Run connects to the primary and serves requests. Reconnects on disconnect.
// Blocks until ctx is cancelled.
//
// Reconnect schedule: 1s → 2s → 4s → 8s → 16s → reconnectBackoffCeiling
// (30s by default), all jittered in [0.75x, 1.25x). Any successful session
// resets backoff to 1s.
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
	connectorBackoffMillis.Set(backoff.Milliseconds())
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
			connectorBackoffMillis.Set(backoff.Milliseconds())
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
					connectorBackoffMillis.Set(backoff.Milliseconds())
				}
			}
		}
		// Jitter the sleep so many connectors restarted together (e.g. fleet
		// SIGHUP) don't hammer the primary on aligned deadlines. backoff
		// still doubles deterministically; we only scatter wall-time.
		timer := time.NewTimer(osutil.JitterBackoff(backoff))
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
				backoff = min(backoff*2, reconnectBackoffCeiling)
				connectorBackoffMillis.Set(backoff.Milliseconds())
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

// marshalResultBufPool reuses bytes.Buffer + json.Encoder pairs so the
// reflect-based encodeState scratch is not freshly allocated for every
// reverse-RPC reply. Each handleRequest path (ListSessions / Discover /
// Preview / EventEntriesSince / Status maps / etc.) calls marshalResult
// at least once, and a busy primary fans out 5-50 calls/s; pooling the
// scratch buffer cuts ~1 alloc/call at this rate. R246-PERF-10.
//
// Buffers larger than marshalResultMaxRetainBytes are dropped on Put so
// a one-off megabyte payload (e.g. a large EventEntriesSince response)
// can't pin retained heap forever for steady-state callers that only
// ever marshal small status maps.
var marshalResultBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

const marshalResultMaxRetainBytes = 64 * 1024

// sanitizeWorkspacePath validates and canonicalises a remote-supplied
// workspace / cwd path. It centralises the EvalSymlinks + Clean + IsAbs
// + allowed-root prefix gate that previously lived inline in the "send",
// "takeover", and "close_discovered" reverse-RPC branches. R237-CR-6 (#709).
//
// Contract:
//   - raw is the remote-supplied path (already non-empty by caller).
//   - kind is the human-readable label used in error messages, e.g.
//     "workspace", "takeover cwd", "close_discovered cwd".
//   - tolerateMissing controls whether fs.ErrNotExist from EvalSymlinks
//     is downgraded to "use the cleaned syntactic path". close_discovered
//     sets this true because the CWD frequently vanishes after the Claude
//     CLI has exited; send/takeover keep it false because they expect the
//     directory to still exist.
//
// Caller responsibility: invoke session.ValidateRemoteWorkspacePath(raw)
// FIRST so syntactic traversal / control-byte / non-absolute inputs are
// rejected before they hit filepath.Clean (which silently folds
// `/home/../etc` into `/etc`). Caller must also enforce the empty-
// defaultWorkspace policy (refuse vs. fall back) since send/takeover
// refuse but close_discovered tolerates — that policy lives at the call
// site, not here. R68-SEC-M2.
//
// Returns the canonical absolute path on success.
func (c *Connector) sanitizeWorkspacePath(raw, kind string, tolerateMissing bool) (string, error) {
	cleaned := filepath.Clean(raw)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		if tolerateMissing && errors.Is(err, fs.ErrNotExist) {
			resolved = cleaned
		} else {
			return "", fmt.Errorf("%s path invalid: %w", kind, err)
		}
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("%s must be absolute path", kind)
	}
	if resolved != c.defaultWorkspace &&
		!strings.HasPrefix(resolved, c.defaultWorkspace+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q outside allowed root %q", kind, resolved, c.defaultWorkspace)
	}
	return resolved, nil
}

func marshalResult(v any) (json.RawMessage, error) {
	buf := marshalResultBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	// json.Encoder.Encode appends a trailing '\n' the upstream RPC reader
	// does not expect; trim before returning so the wire format matches
	// the prior json.Marshal output exactly.
	if err := enc.Encode(v); err != nil {
		// On error, drop the buffer (potentially partially-written) so a
		// poison value cannot leak into the next reuse.
		marshalResultBufPool.Put(new(bytes.Buffer))
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	// Copy out so the returned RawMessage is decoupled from the pooled
	// buffer (the next Put would clobber it via Reset).
	cp := append(json.RawMessage(nil), out...)
	if buf.Cap() <= marshalResultMaxRetainBytes {
		marshalResultBufPool.Put(buf)
	} else {
		// Drop oversized buffers so a single huge response doesn't pin
		// retained heap for the lifetime of the pool.
		marshalResultBufPool.Put(new(bytes.Buffer))
	}
	return cp, nil
}
