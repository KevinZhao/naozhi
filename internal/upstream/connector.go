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
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// handleConnDrainBudget bounds the deferred wg.Wait() at the end of
// handleConn. Workers honour connCtx; the budget covers a downstream call
// (e.g. sess.Send blocked on the CLI watchdog, ≈5 min) that refuses to
// unblock — the stuck goroutine leaks to process teardown rather than
// pinning the reconnect loop. Package-level var so tests can shorten it.
var handleConnDrainBudget = 15 * time.Second

// circuitBreakerThreshold is the number of consecutive runOnce failures
// that trips the circuit breaker. With the 1s→30s doubling schedule, 6
// failures cover ≈1 minute before the longer breaker backoff kicks in.
var circuitBreakerThreshold = 6

// reconnectBackoffCeiling caps the doubling reconnect backoff
// (1s → 2s → 4s → 8s → 16s → 30s, jittered). The breaker kicks in past
// this ceiling, so keep it readable next to circuitBreakerBackoff.
// Package-level var so tests can shorten it.
var reconnectBackoffCeiling = 30 * time.Second

// circuitBreakerBackoff is the backoff floor once the breaker trips. 5 min
// is short enough that transient outages (DNS hiccup, primary restart, cert
// rollover) still auto-recover, but long enough to cut log noise versus the
// 30s reconnectBackoffCeiling. Package-level var so tests can shorten it.
var circuitBreakerBackoff = 5 * time.Minute

// reasonSessionReset is the Reason on the terminal session_state message
// streamEvents emits when the router has already dropped the session.
// Downstream consumers (reverseconn.go, dashboard.js) match this literal.
const reasonSessionReset = "session_reset"

// discoverFn is the callback type behind Connector.discoverFunc; a named
// type so atomic.Pointer can box it.
type discoverFn func() (json.RawMessage, error)

// previewFn is the callback type behind Connector.previewFunc.
type previewFn func(sessionID string) (json.RawMessage, error)

// Config is the upstream-local value shape New consumes. The cmd boundary
// translates config.UpstreamConfig → upstream.Config so this bottom-of-DAG
// package does not import internal/config (#1411).
type Config struct {
	URL         string
	NodeID      string
	Token       string
	DisplayName string
	Insecure    bool
}

// Connector dials a primary naozhi and serves it as a reverse-connected node.
// Run on machines behind NAT that cannot be reached by the primary directly.
type Connector struct {
	cfg *Config
	// router is the SessionRouter subset used by Connector (consumer.go).
	router  SessionRouter
	projMgr *project.Manager // may be nil
	// resolver derives planner-view opts for reverse-RPC restart_planner
	// (docs/rfc/key-resolver.md Phase 5); nil falls back to inline AgentOpts.
	resolver         *session.KeyResolver
	claudeDir        string
	hostname         string
	defaultWorkspace string // used as allowedRoot for incoming workspace overrides
	// discoverFunc / previewFunc are atomic so SetDiscoverFunc and
	// handleRequest may run concurrently; Load returns nil when never set.
	discoverFunc atomic.Pointer[discoverFn]
	previewFunc  atomic.Pointer[previewFn]
}

// New creates a Connector. projMgr may be nil if projects are not configured;
// resolver may be nil (restart_planner then uses the inline AgentOpts path).
func New(cfg *Config, router *session.Router, projMgr *project.Manager, resolver *session.KeyResolver) *Connector {
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
// Safe to call concurrently with handleRequest; nil clears the callback
// (the RPC then returns an empty array).
func (c *Connector) SetDiscoverFunc(fn func() (json.RawMessage, error)) {
	if fn == nil {
		c.discoverFunc.Store(nil)
		return
	}
	boxed := discoverFn(fn)
	c.discoverFunc.Store(&boxed)
}

// SetPreviewFunc sets a callback that returns conversation history for a discovered session.
// Same semantics as SetDiscoverFunc; nil clears the callback.
func (c *Connector) SetPreviewFunc(fn func(sessionID string) (json.RawMessage, error)) {
	if fn == nil {
		c.previewFunc.Store(nil)
		return
	}
	boxed := previewFn(fn)
	c.previewFunc.Store(&boxed)
}

// loadDiscoverFunc returns the current discover callback, or nil if none was installed.
func (c *Connector) loadDiscoverFunc() discoverFn {
	if p := c.discoverFunc.Load(); p != nil {
		return *p
	}
	return nil
}

// loadPreviewFunc returns the current preview callback, or nil if none was installed.
func (c *Connector) loadPreviewFunc() previewFn {
	if p := c.previewFunc.Load(); p != nil {
		return *p
	}
	return nil
}

// Run connects to the primary and serves requests. Reconnects on disconnect.
// Blocks until ctx is cancelled.
//
// Reconnect schedule: 1s → 2s → … → reconnectBackoffCeiling, all jittered in
// [0.75x, 1.25x); any successful session resets backoff to 1s. After
// circuitBreakerThreshold consecutive failures the floor jumps to
// circuitBreakerBackoff with a single breaker-tripped WARN; the first
// success clears it. The per-attempt "connector disconnected" WARN still fires.
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
		// A "successful session" means runOnce returned connected=true, even
		// if the eventual disconnect surfaced an error.
		if connected {
			consecutiveFailures = 0
			if circuitTripped {
				slog.Info("connector circuit breaker reset after successful connection", "url", c.cfg.URL)
				circuitTripped = false
			}
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
		// Jitter so many connectors restarted together (e.g. fleet SIGHUP)
		// don't hammer the primary on aligned deadlines.
		timer := time.NewTimer(osutil.JitterBackoff(backoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// Double only within the normal ceiling; once tripped, backoff stays
			// pinned at circuitBreakerBackoff until the next successful connect.
			if backoff < circuitBreakerBackoff {
				backoff = min(backoff*2, reconnectBackoffCeiling)
				connectorBackoffMillis.Set(backoff.Milliseconds())
			}
		}
	}
}

// connectorTLSConfig builds the TLS config for the wss:// dial. The 1.2 floor
// is always pinned so a compromised segment cannot force a weaker protocol;
// insecure (operator opt-in via upstream.insecure=true) skips certificate
// verification only (#1711).
func connectorTLSConfig(insecure bool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecure, //nolint:gosec // gated by upstream.insecure config flag (validated)
	}
}

func (c *Connector) runOnce(ctx context.Context) (bool, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// TLS 1.2 floor pinned; insecure mode only skips cert verification (see connectorTLSConfig).
		TLSClientConfig: connectorTLSConfig(c.cfg.Insecure),
	}
	conn, _, dialErr := dialer.DialContext(ctx, c.cfg.URL, nil)
	if dialErr != nil {
		return false, fmt.Errorf("dial: %w", dialErr)
	}
	// Surface operator signal when the token goes over plaintext ws://
	// (requires upstream.insecure=true). One warn per successful dial.
	if strings.HasPrefix(c.cfg.URL, "ws://") {
		slog.Warn("upstream connector: transmitting token over plaintext ws:// — set upstream.insecure=false and use wss:// for production")
	}
	// Same signal for a wss:// dial with certificate verification disabled (#1711).
	if c.cfg.Insecure && strings.HasPrefix(c.cfg.URL, "wss://") {
		slog.Warn("upstream connector: TLS certificate verification disabled (upstream.insecure=true) — set upstream.insecure=false for production")
	}
	// Bound inbound frame size so a malicious or buggy primary cannot exhaust
	// memory; matches the primary side's ReverseConn limit (#2084).
	conn.SetReadLimit(limits.MaxStreamJSONLine)

	// gorilla/websocket does not document concurrent Close calls; the
	// cancel-watchdog goroutine below and the deferred close would race, so
	// serialize both through a sync.Once.
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

	// Register. Capabilities are derived from the locally-registered
	// backend.Profile set (union of RequiredNodeCaps, sorted for deterministic
	// wire output) so the primary's selectNodeForBackend needs no operator caps.
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

	// A SetReadDeadline error means the net.Conn is already torn down;
	// ReadJSON would block forever without a deadline.
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
		// Surface the error so the outer ReadJSON loop exits instead of blocking.
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		return false, fmt.Errorf("set initial read deadline: %w", err)
	}

	return true, c.handleConn(ctx, conn)
}

// pingOnce sends one WebSocket-level ping under writeMu and closes the conn
// on any failure. Returns false if the conn was torn down (caller returns).
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

// marshalResultBufPool reuses bytes.Buffer + json.Encoder scratch across
// reverse-RPC replies (a busy primary fans out 5-50 calls/s). Buffers above
// marshalResultMaxRetainBytes are dropped on Put so one large payload cannot
// pin retained heap for steady-state small replies.
var marshalResultBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

const marshalResultMaxRetainBytes = 64 * 1024

// sanitizeWorkspacePath validates and canonicalises a remote-supplied
// workspace / cwd path: EvalSymlinks + Clean + IsAbs + allowed-root prefix
// gate shared by the send / takeover / close_discovered branches (#709).
// kind labels error messages; tolerateMissing downgrades fs.ErrNotExist from
// EvalSymlinks to the cleaned syntactic path (close_discovered runs after
// the CLI exited and its CWD may be gone).
//
// Callers must run session.ValidateRemoteWorkspacePath(raw) FIRST so
// traversal / control-byte / relative inputs are rejected before Clean folds
// `/home/../etc` into `/etc`, and must apply their own empty-defaultWorkspace policy.
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
	// Encode appends a trailing '\n' the RPC reader does not expect; trimmed below.
	if err := enc.Encode(v); err != nil {
		// Reset clears partial output, so the buffer is safe to return to the
		// pool (same oversized-drop policy as the happy path).
		buf.Reset()
		if buf.Cap() <= marshalResultMaxRetainBytes {
			marshalResultBufPool.Put(buf)
		} else {
			marshalResultBufPool.Put(new(bytes.Buffer))
		}
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
		marshalResultBufPool.Put(new(bytes.Buffer))
	}
	return cp, nil
}
