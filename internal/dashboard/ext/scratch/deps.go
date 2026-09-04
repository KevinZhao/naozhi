// Package scratch hosts the dashboard /api/scratch/* endpoints used by the
// "aside" drawer (preview-pane chat seeded with quoted context).
package scratch

import (
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/session"
)

// Broadcaster is the subset of *server.Hub the scratch handler uses to nudge
// the sidebar after open/delete/promote, without reverse-importing server.
type Broadcaster interface {
	BroadcastSessionsUpdate()
}

// ScratchRouter is the *Handler-only subset of *session.Router (mirrors
// internal/server/consumer.go); three methods cover open/promote/delete.
type ScratchRouter interface {
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
}

// IPLimiter aliases the shared dashboard contract (#2285).
type IPLimiter = contracts.IPLimiter
