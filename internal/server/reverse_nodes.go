package server

import "github.com/naozhi/naozhi/internal/node"

// attachReverseNodeServer wires the reverse-connection server's node lifecycle
// into this server's node registry, node cache and hub: a node that registers
// becomes known and gets its sessions fetched; one that deregisters is purged
// from the registry, the cache and every subscription, and the dashboard is
// told its session list changed. nil (no reverse server configured) is a no-op.
func (s *Server) attachReverseNodeServer(rs *node.ReverseServer) {
	if rs == nil {
		return
	}
	s.reverseNodeServer = rs
	for id, displayName := range rs.AllNodes() {
		s.nodes.SetKnown(id, displayName)
	}
	rs.OnRegister = func(id string, rc *node.ReverseConn) {
		s.nodes.Add(id, rc)
		go s.nodeCache.RefreshFor(id) // RefreshFor calls onChange → BroadcastSessionsUpdate
	}
	rs.OnDeregister = func(id string) {
		s.nodes.Remove(id)
		s.nodeCache.PurgeNode(id)
		s.hub.PurgeNodeSubscriptions(id)
		s.hub.BroadcastSessionsUpdate()
	}
}
