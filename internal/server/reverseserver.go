package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/reverse"
)

// ReverseNodeServer accepts /ws-node connections from remote naozhi nodes.
// Remote nodes dial in (reverse connect) to traverse NAT.
type ReverseNodeServer struct {
	mu    sync.RWMutex
	auth  map[string]string           // node_id → expected token
	names map[string]string           // node_id → configured display_name
	conns map[string]*ReverseNodeConn // node_id → active connection

	onRegister   func(id string, conn *ReverseNodeConn)
	onDeregister func(id string)
}

// NewReverseNodeServer creates a server that accepts /ws-node connections.
// auth is the reverse_nodes config from config.yaml.
func NewReverseNodeServer(auth map[string]config.ReverseNodeEntry) *ReverseNodeServer {
	tokens := make(map[string]string, len(auth))
	names := make(map[string]string, len(auth))
	for id, e := range auth {
		tokens[id] = e.Token
		names[id] = e.DisplayName
	}
	return &ReverseNodeServer{
		auth:  tokens,
		names: names,
		conns: make(map[string]*ReverseNodeConn),
	}
}

// ServeHTTP handles the /ws-node WebSocket endpoint.
func (s *ReverseNodeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws-node upgrade failed", "err", err)
		return
	}

	// Read register message with timeout
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var msg reverse.ReverseMsg
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
		conn.WriteJSON(reverse.ReverseMsg{Type: "register_fail", Error: "auth failed"}) //nolint
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

	rc := newReverseNodeConn(msg.NodeID, displayName, r.RemoteAddr, conn)
	if err := conn.WriteJSON(reverse.ReverseMsg{Type: "registered"}); err != nil {
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

	if s.onRegister != nil {
		s.onRegister(msg.NodeID, rc)
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
		if s.onDeregister != nil {
			s.onDeregister(msg.NodeID)
		}
	}()
	// ServeHTTP returns; rc.readLoop keeps the WS alive on its own goroutine.
}

// Get returns the active connection for a node, or nil.
func (s *ReverseNodeServer) Get(id string) *ReverseNodeConn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conns[id]
}

// AllNodes returns all configured node IDs mapped to their display names.
// Includes disconnected nodes.
func (s *ReverseNodeServer) AllNodes() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]string, len(s.names))
	for id, name := range s.names {
		result[id] = name
	}
	return result
}
