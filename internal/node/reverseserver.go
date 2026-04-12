package node

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
)

// reverseUpgrader is the WebSocket upgrader for reverse node connections.
// CheckOrigin always returns true because auth is enforced via per-node
// bearer token in the first WebSocket message, not via CORS.
var reverseUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ReverseServer accepts /ws-node connections from remote naozhi nodes.
// Remote nodes dial in (reverse connect) to traverse NAT.
type ReverseServer struct {
	mu    sync.RWMutex
	auth  map[string]string       // node_id → expected token
	names map[string]string       // node_id → configured display_name
	conns map[string]*ReverseConn // node_id → active connection

	OnRegister   func(id string, conn *ReverseConn)
	OnDeregister func(id string)
}

// NewReverseServer creates a server that accepts /ws-node connections.
// auth is the reverse_nodes config from config.yaml.
func NewReverseServer(auth map[string]config.ReverseNodeEntry) *ReverseServer {
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
	}
}

// ServeHTTP handles the /ws-node WebSocket endpoint.
func (s *ReverseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := reverseUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws-node upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(16 << 20) // 16 MB — node RPC payloads can include full session responses

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
		slog.Warn("reverse node auth failed", "ip", r.RemoteAddr, "node_id", msg.NodeID)
		conn.WriteJSON(ReverseMsg{Type: "register_fail", Error: "auth failed"}) //nolint
		conn.Close()
		return
	}

	// Use configured display name; fall back to what remote sent
	displayName := s.names[msg.NodeID]
	if displayName == "" {
		displayName = msg.DisplayName
	}
	if displayName == "" {
		displayName = msg.NodeID
	}

	remoteLabel := msg.Hostname
	if remoteLabel == "" {
		remoteLabel = r.RemoteAddr
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

	slog.Info("reverse node registered", "node_id", msg.NodeID, "ip", r.RemoteAddr)

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
