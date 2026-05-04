package node

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/netutil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// truncateLabelUTF8 truncates s to at most max bytes while preserving UTF-8
// validity and stripping log-injection codepoints. Raw byte truncation is
// unsafe when the cut falls mid-rune: the resulting string contains invalid
// UTF-8 bytes that flow into slog attrs, JSON responses, and dashboard
// renders. `strings.ToValidUTF8` strips any trailing invalid-byte fragment
// after a byte-level cut, keeping the rest intact. R67-SEC-6.
//
// R180-SEC-M2: also strip C0/C1/bidi/LS-PS codepoints. A compromised reverse
// node (with a valid token) could submit display_name / hostname containing
// bidi overrides to flip the rendered name on every dashboard (/api/sessions
// stats.nodes), or C1/newline bytes to corrupt slog attrs when the node
// registers / disconnects. Mirrors the cron-validator and project-name policy.
func truncateLabelUTF8(s string, max int) string {
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "")
	}
	if s == "" {
		return s
	}
	// Fast path: pure ASCII-printable is already safe.
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if osutil.IsLogInjectionRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// reverseUpgrader is the WebSocket upgrader for reverse node connections.
// m2m connection: bearer token in the first WS message is the primary auth.
// As a defence-in-depth measure, reject any request that carries an Origin
// header — browsers always send Origin, machine-to-machine clients do not.
var reverseUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == ""
	},
}

// ReverseServer accepts /ws-node connections from remote naozhi nodes.
// Remote nodes dial in (reverse connect) to traverse NAT.
type ReverseServer struct {
	mu    sync.RWMutex
	auth  map[string]string       // node_id → expected token
	names map[string]string       // node_id → configured display_name
	conns map[string]*ReverseConn // node_id → active connection

	// wsLimiter is an internal per-IP rate limiter store for /ws-node connections.
	// Separate from the dashboard login limiter to prevent cross-endpoint interference.
	// Higher burst (10) than login (5) since machine-to-machine reconnects are bursty.
	wsLimiter *ratelimit.Limiter

	// trustedProxy enables X-Forwarded-For last-hop IP extraction for rate limiting.
	// When true (ALB/CloudFront in front), per-IP limits apply to the real client,
	// not the proxy's single IP.
	trustedProxy bool

	OnRegister   func(id string, conn *ReverseConn)
	OnDeregister func(id string)
}

// NewReverseServer creates a server that accepts /ws-node connections.
// auth is the reverse_nodes config from config.yaml.
// trustedProxy enables X-Forwarded-For last-hop IP extraction so per-IP
// rate limiting works correctly when deployed behind ALB/CloudFront.
func NewReverseServer(auth map[string]config.ReverseNodeEntry, trustedProxy bool) *ReverseServer {
	tokens := make(map[string]string, len(auth))
	names := make(map[string]string, len(auth))
	for id, e := range auth {
		tokens[id] = e.Token
		names[id] = e.DisplayName
	}
	return &ReverseServer{
		auth:  tokens,
		names: names,
		conns: make(map[string]*ReverseConn),
		wsLimiter: ratelimit.New(ratelimit.Config{
			Rate:  rate.Every(5 * time.Second), // 1 per 5s sustained
			Burst: 10,                          // 10 burst
		}),
		trustedProxy: trustedProxy,
	}
}

// ServeHTTP handles the /ws-node WebSocket endpoint.
func (s *ReverseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit to prevent token brute-force via rapid connect cycles.
	// Uses trusted-proxy-aware client IP so ALB-fronted deployments limit the
	// real caller, not the single ALB IP.
	ip := netutil.ClientIP(r, s.trustedProxy)
	// Fallback to a shared bucket if IP resolution failed so ratelimit's
	// empty-key hard-reject doesn't 429 a legitimate client forever.
	limitKey := ip
	if limitKey == "" {
		limitKey = "_unknown_"
	}
	if !s.wsLimiter.Allow(limitKey) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	conn, err := reverseUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws-node upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(4 << 10) // 4 KB — small limit for unauthenticated register message

	// Read register message with timeout. R182-GO-P1-1: both SetReadDeadline
	// returns were previously dropped. If the underlying net.Conn is already
	// half-closed mid-handshake, SetReadDeadline fails and ReadJSON would
	// block forever (deadline-less on a dead socket), leaking a goroutine
	// on every failed handshake from the public /ws-node endpoint. Treat
	// failure as "connection unusable" and bail fast, mirroring the
	// symmetric SetWriteDeadline check at line 195.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		slog.Debug("ws-node: set read deadline failed", "err", err)
		conn.Close()
		return
	}
	var msg ReverseMsg
	if err := conn.ReadJSON(&msg); err != nil || msg.Type != "register" {
		conn.Close()
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Debug("ws-node: clear read deadline failed", "err", err)
		conn.Close()
		return
	}

	// Validate token — constant-time comparison to prevent timing oracle.
	// Generic error to avoid node_id enumeration.
	expected, ok := s.auth[msg.NodeID]
	if !ok || expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(msg.Token)) != 1 {
		// R180-SEC-H2 / R181-GO-P2-1: msg.NodeID comes from an unauthenticated
		// 4 KB frame on the public /ws-node endpoint. Anyone can probe with
		// arbitrary bytes. SanitizeForLog keeps attr values machine-readable
		// (strips C0/C1/bidi/LS-PS → '_') instead of the earlier %q path
		// which produced Go-quoted strings that then got double-escaped by
		// slog's JSON handler.
		slog.Warn("reverse node auth failed", "ip", ip, "node_id", osutil.SanitizeForLog(msg.NodeID, 64))
		conn.WriteJSON(ReverseMsg{Type: "register_fail", Error: "auth failed"}) //nolint
		conn.Close()
		return
	}

	// Auth succeeded — raise limit for RPC payloads (session responses, event batches).
	conn.SetReadLimit(16 << 20) // 16 MB

	// Use configured display name; fall back to what remote sent.
	// Cap remote-supplied strings so a compromised worker cannot bloat the
	// dashboard /api/sessions payload (defense-in-depth after token auth).
	const maxLabel = 256
	displayName := s.names[msg.NodeID]
	if displayName == "" {
		displayName = msg.DisplayName
	}
	if displayName == "" {
		displayName = msg.NodeID
	}
	displayName = truncateLabelUTF8(displayName, maxLabel)

	remoteLabel := msg.Hostname
	if remoteLabel == "" {
		remoteLabel = r.RemoteAddr
	}
	remoteLabel = truncateLabelUTF8(remoteLabel, maxLabel)
	rc := newReverseConn(msg.NodeID, displayName, remoteLabel, conn)
	// Bound the register response write so a slow-read attacker can't
	// park this goroutine indefinitely at the TCP window. newReverseConn
	// applies 10s per write thereafter; this pre-handoff write needs the
	// same protection. If SetWriteDeadline fails (conn closed mid-handshake),
	// abort before WriteJSON would block deadline-less on a dead socket.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		rc.Close()
		return
	}
	if err := conn.WriteJSON(ReverseMsg{Type: "registered"}); err != nil {
		rc.Close()
		return
	}
	// R183-GO-M1: clearing the write deadline can only fail on a broken /
	// half-closed socket. Silently dropping the error mirrors a bug fixed
	// symmetrically at line 144/155 for SetReadDeadline (R182-GO-P1-1):
	// per-write deadline resets in newReverseConn's writePump also fail,
	// and without a deadline, a subsequent WriteJSON on this conn can
	// block until TCP keepalive expires (minutes). Treat failure as
	// "connection unusable", tear down, and bail.
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("ws-node: clear write deadline failed", "err", err)
		rc.Close()
		return
	}

	s.mu.Lock()
	if old, exists := s.conns[msg.NodeID]; exists {
		old.Close()
	}
	s.conns[msg.NodeID] = rc
	s.mu.Unlock()

	// R181-SEC-P2-1: authenticated node_id matched a config key, but those
	// keys are never run through truncateLabelUTF8 on load — an operator
	// typo in config.yaml with a bidi/C1/newline char would reach slog
	// attrs verbatim. Symmetric with the auth-failed path and cheap.
	safeNodeID := truncateLabelUTF8(msg.NodeID, 64)
	slog.Info("reverse node registered", "node_id", safeNodeID, "ip", ip)

	if s.OnRegister != nil {
		// msg.NodeID is kept verbatim here so downstream state
		// (`s.conns[msg.NodeID]`, Server.nodes, knownNodes) is keyed with
		// the authenticated-config id; OnDeregister below must pass the
		// same value so the map entries round-trip correctly. Sanitizing
		// only the slog.Info label (safeNodeID above) matches R181-SEC-P2-1.
		s.OnRegister(msg.NodeID, rc)
	}

	go rc.readLoop()

	// Wait for disconnect, then deregister
	go func() {
		<-rc.done
		s.mu.Lock()
		if s.conns[msg.NodeID] == rc {
			delete(s.conns, msg.NodeID)
		}
		s.mu.Unlock()
		slog.Info("reverse node disconnected", "node_id", safeNodeID)
		if s.OnDeregister != nil {
			s.OnDeregister(msg.NodeID)
		}
	}()
	// ServeHTTP returns; rc.readLoop keeps the WS alive on its own goroutine.
}

// AllNodes returns all configured node IDs mapped to their display names.
// Includes disconnected nodes.
func (s *ReverseServer) AllNodes() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]string, len(s.names))
	for id, name := range s.names {
		result[id] = name
	}
	return result
}
