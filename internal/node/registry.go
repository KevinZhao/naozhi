// NodeMeta carries per-connection metadata so server-side routing can answer
// "does this node support backend X?". Reverse nodes self-report Capabilities
// at register time; HTTPClient nodes carry an empty set. Unknown tags are
// stored verbatim (forward-compat, see caps.go).
package node

import "time"

// NodeMeta describes a connected reverse node; immutable for the life of the
// connection. Capabilities is the O(1) lookup form consulted via HasCap.
type NodeMeta struct {
	NodeID       string
	DisplayName  string
	Hostname     string
	Capabilities map[string]bool
	RegisteredAt time.Time
}

// HasCap reports whether the node advertised cap. An empty cap is "no
// requirement" and always satisfied (claude has nil RequiredNodeCaps and must
// match legacy peers). nil-receiver safe.
func (m *NodeMeta) HasCap(cap string) bool {
	if cap == "" {
		return true
	}
	if m == nil || m.Capabilities == nil {
		return false
	}
	return m.Capabilities[cap]
}

// capsFromSlice converts the wire slice into the lookup map, coalescing
// empty/duplicate entries; nil for an empty input.
func capsFromSlice(caps []string) map[string]bool {
	if len(caps) == 0 {
		return nil
	}
	out := make(map[string]bool, len(caps))
	for _, c := range caps {
		if c == "" {
			continue
		}
		out[c] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
