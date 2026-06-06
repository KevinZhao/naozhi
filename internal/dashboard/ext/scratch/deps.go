// Package scratch hosts the dashboard /api/scratch/* endpoints used by the
// "aside" drawer (preview-pane chat seeded with quoted context). Phase 3c
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package scratch

import (
	"net/http"

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

// IPLimiter is the subset of internal/server.ipLimiter the scratch handler
// uses. server's *ipLimiter satisfies this shape; we accept the interface
// so the sub-package doesn't reverse-import server.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}
