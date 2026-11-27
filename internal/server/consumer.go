// consumer.go: per-consumer interface subsets of *session.Router (HubRouter /
// sendEngineRouter / serverRouter / healthRouter) and *wsBroadcaster (sendNotifier). Declared here (not in
// session) so tests can inject fakes; the concrete types satisfy them
// structurally; the wiring-site assignments check that at compile time. See
// docs/rfc/consumer-interfaces.md §3.2.2.

package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/wsproto"

	"github.com/naozhi/naozhi/internal/session"
)

// HubRouter is the subset of *session.Router that *Hub consumes on the
// WebSocket subscribe / send / interrupt paths, plus the h.hub.router.*
// transits *ScratchHandler / *SendHandler borrow. Rethink the shape if it
// grows past 15 methods (docs/rfc/consumer-interfaces.md §7.2).
// *session.Router satisfies it structurally.
type HubRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	SessionFor(key string) *session.ManagedSession
	Remove(key string) bool
	RenameSession(oldKey, newKey string) bool
	ResetAndDiscardOverride(key string)
	Workspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	SetSessionBackend(key, backend string)
	SetSessionAccessProfile(key, profile string)
	DefaultWorkspace() string
	RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)
	InterruptSessionSafe(key string) session.InterruptOutcome
	InterruptSessionViaControl(key string) session.InterruptOutcome
	NotifyIdle()
}

// sendEngineRouter is the *sendEngine-only subset of *session.Router: the
// methods the send pipeline calls to validate and record a send's overrides,
// plus GetOrCreate for a relayed send's preflight (relay_origin.go). Turns
// reach sessions through turnSender (turnRouter), the one turn.Sender every
// entry's turns go through. Deliberately not HubRouter — that
// one is at 14 methods (near the consumer-interfaces.md §7.2 rethink threshold) and
// its godoc admits it carries the transits *SendHandler borrows, so handing it
// to the engine would just move the borrowing debt to a new holder.
// *session.Router satisfies it structurally. It is also the ONLY router handle on the HTTP send path (#2632).
type sendEngineRouter interface {
	SessionFor(key string) *session.ManagedSession
	Workspace(chatKey string) string
	SetWorkspace(chatKey, path string)
	SetSessionBackend(key, backend string)
	SetSessionAccessProfile(key, profile string)
	DefaultWorkspace() string
	RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
}

// sendNotifier is *sendEngine's only way out to the dashboard, and the
// hand-off point for Epic K (#2549): the broadcast facet is its own type,
// *wsBroadcaster, so the engine depends on it and not on the Hub.
// *wsBroadcaster satisfies it.
type sendNotifier interface {
	BroadcastSessionReady(key string)
	BroadcastSessionsUpdate()
	broadcastState(key, state, deathReason string)
	broadcastSendError(key, msg string)
	broadcastRemoteSendError(f wsproto.SendError)
}

// serverRouter is the subset of *session.Router *Server's runtime methods
// use: IM auto-takeover (takeover.go), the project-scan loop and the reply
// footer default. The build steps take the concrete pointer from
// wiring.router instead.
type serverRouter interface {
	Backends() *session.BackendRegistry
	SessionFor(key string) *session.ManagedSession
	Workspace(chatKey string) string
	ManagedExcludeSets() (pids map[int]bool, sessionIDs map[string]bool, cwds map[string]bool)
	ReserveTakeover(key string, opts session.AgentOpts) (*session.TakeoverLease, error)
	Takeover(ctx context.Context, lease *session.TakeoverLease, sessionID string, workspace string) (*session.ManagedSession, error)
	Remove(key string) bool
	BumpVersion()
}

// healthRouter is the subset of *session.Router *HealthHandler and its
// probes (health_probe.go) read.
type healthRouter interface {
	Stats() (active, total int)
	Backends() *session.BackendRegistry
	History() *session.HistoryIO
	StoreWriteBlocks() []session.StoreBlock
}
