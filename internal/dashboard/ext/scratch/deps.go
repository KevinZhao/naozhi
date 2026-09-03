// Package scratch hosts the dashboard /api/scratch/* endpoints used by the
// "aside" drawer (preview-pane chat seeded with quoted context). Phase 3c
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package scratch

import (
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/session"
)

// Broadcaster is the subset of *server.Hub the scratch handler uses —
// just BroadcastSessionsUpdate to nudge the dashboard sidebar after open/
// delete/promote. Defining the interface here (per "accept interfaces"
// idiom) lets server inject *server.Hub without the sub-package
// reverse-importing it.
type Broadcaster interface {
	BroadcastSessionsUpdate()
}

// ScratchRouter is the *Handler-only subset of *session.Router. Mirrors
// internal/server/consumer.go's ScratchRouter so the sub-package doesn't
// reverse-import server. Three methods cover open/promote/delete.
type ScratchRouter interface {
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
}

// IPLimiter aliases the shared dashboard contract (#2285); server's
// *ipLimiter is injected without a reverse import.
type IPLimiter = contracts.IPLimiter
