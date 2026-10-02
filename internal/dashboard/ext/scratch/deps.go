// Package scratch hosts the dashboard /api/scratch/* endpoints used by the
// "aside" drawer (preview-pane chat seeded with quoted context).
package scratch

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Broadcaster is the subset of the server's WebSocket broadcaster the scratch
// handler uses to nudge the sidebar after open/delete/promote, without
// reverse-importing server.
type Broadcaster interface {
	BroadcastSessionsUpdate()
}

// ScratchRouter is the *Handler-only subset of *session.Router (mirrors
// internal/server/consumer.go); three methods cover open/promote/delete. The
// router's own SessionFor returns the concrete session, so the wiring site
// adapts it; a missing session must arrive as a nil interface.
type ScratchRouter interface {
	SessionFor(key string) SourceSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
}

// SourceSession is what opening an aside reads off the quoted session: its
// snapshot (agent and tuning to inherit) and the events around the quote.
type SourceSession interface {
	Snapshot() sessionview.SessionSnapshot
	EventEntriesBeforeCtx(ctx context.Context, beforeMS int64, limit int) []clievent.EventEntry
	EventEntriesSince(afterMS int64) []clievent.EventEntry
	EventLastN(n int) []clievent.EventEntry
}

// IPLimiter aliases the shared dashboard contract (#2285).
type IPLimiter = contracts.IPLimiter
