package node

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/netutil"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

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

	// Read register message with timeout
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var msg ReverseMsg
	if err := conn.ReadJSON(&msg); err != nil || msg.Type != "register" {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})

	// Validate token — constant-time comparison to prevent timing oracle.
	// Generic error to avoid node_id enumeration.
	expected, ok := s.auth[msg.NodeID]
	if !ok || expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(msg.Token)) != 1 {
		slog.Warn("reverse node auth failed", "ip", ip, "node_id", msg.NodeID)
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
	if len(displayName) > maxLabel {
		displayName = displayName[:maxLabel]
	}

	remoteLabel := msg.Hostname
	if remoteLabel == "" {
		remoteLabel = r.RemoteAddr
	}
	if len(remoteLabel) > maxLabel {
		remoteLabel = remoteLabel[:maxLabel]
	}
	rc := newReverseConn(msg.NodeID, displayName, remoteLabel, conn)
	if err := conn.WriteJSON(ReverseMsg{Type: "registered"}); err != nil {
		rc.Close()
		return
	}

	s.mu.Lock()
	if old, exists := s.conns[msg.NodeID]; exists {
		old.Close()
	}
	s.conns[msg.NodeID] = rc
	s.mu.Unlock()

	slog.Info("reverse node registered", "node_id", msg.NodeID, "ip", ip)

	if s.OnRegister != nil {
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
		slog.Info("reverse node disconnected", "node_id", msg.NodeID)
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
