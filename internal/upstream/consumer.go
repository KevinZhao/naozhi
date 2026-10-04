// Package upstream — consumer.go
//
// SessionRouter is what Connector needs from the session router when
// translating primary-reverse RPC into local router operations. It speaks in
// upstream's own Session interface and sessionview's value types, so this
// package never imports internal/session: the router's concrete types can
// change without reaching the connector. internal/wireup adapts
// *session.Router to it (upstream_router.go, which pins the method set).
// It is composed from narrow sub-interfaces so consumer code can depend on
// the smallest capability it actually needs.
package upstream

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Session is the part of a session the connector drives: it sends into the
// session, follows its events and reports its state. A router method finds no
// session by returning a nil Session.
type Session interface {
	Send(ctx context.Context, text string, images []clievent.Attachment, onEvent clievent.EventCallback) (*clievent.SendResult, error)
	SubscribeEvents() (<-chan struct{}, func())
	EventEntriesSince(afterMS int64) []clievent.EventEntry
	// InitialHistoryPage is the opening page a want_history subscribe gets:
	// the visible-aware newest slice for a page-size hint, and whether older
	// history exists.
	InitialHistoryPage(ctx context.Context, limit int) ([]clievent.EventEntry, bool)
	// EventPageBeforeCtx is a "load earlier" page: the newest limit entries
	// older than beforeMS, and whether older history exists.
	EventPageBeforeCtx(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, bool)
	LogSystemEvent(summary string)
	Snapshot() sessionview.SessionSnapshot
	State() string
	DeathReason() string
}

// PlannerResolver derives the key and spawn options of a project's planner
// (docs/rfc/key-resolver.md Phase 5).
type PlannerResolver interface {
	ResolveForPlannerKey(projectName string) (key string, opts sessionview.AgentOpts, ok bool)
}

// SessionLookup is the read-only lookup sub-capability used by hot RPC paths
// (subscribe stream filter, ListSessions response, SessionFor before send).
type SessionLookup interface {
	SessionFor(key string) Session
	ListSessions() []sessionview.SessionSnapshot
}

// SessionLifecycle is the create/recreate/remove sub-capability used by RPC
// handlers that allocate or tear down sessions.
type SessionLifecycle interface {
	GetOrCreate(ctx context.Context, key string, opts sessionview.AgentOpts) (Session, sessionview.SessionStatus, error)
	ResetAndRecreate(ctx context.Context, key string, opts sessionview.AgentOpts) (Session, error)
	Takeover(ctx context.Context, key string, sessionID string, workspace string, opts sessionview.AgentOpts) (Session, error)
	Remove(key string) bool
	DefaultWorkspace() string
}

// SessionMutator is the in-place mutation sub-capability (interrupt, label
// update): mutators preserve session identity while lifecycle ops swap or
// destroy the underlying session.
type SessionMutator interface {
	InterruptSessionSafe(key string) sessionview.InterruptOutcome
	SetUserLabel(key, label string) bool
}

// SessionBackends is the read-only backend-manifest sub-capability used by the
// "fetch_backends" reverse-RPC branch; it assembles the same
// {backends, default, detected} payload GET /api/cli/backends serves locally.
type SessionBackends interface {
	BackendsManifest(detected []cli.BackendInfo) sessionview.BackendManifest
}

// SessionRouter is what the Connector needs from the router, composed from
// the four narrow sub-interfaces above.
type SessionRouter interface {
	SessionLookup
	SessionLifecycle
	SessionMutator
	SessionBackends
}
