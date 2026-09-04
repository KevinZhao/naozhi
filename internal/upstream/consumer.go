// Package upstream — consumer.go
//
// SessionRouter is the subset of *session.Router that Connector uses when
// translating primary-reverse RPC into local router operations. Declared
// here (not in session) so Connector tests can inject a fake; *session.Router
// satisfies it implicitly, pinned by internal/session/contract_test.go.
// It is composed from narrow sub-interfaces so consumer code can depend on
// the smallest capability it actually needs.
package upstream

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// SessionLookup is the read-only lookup sub-capability used by hot RPC paths
// (subscribe stream filter, ListSessions response, SessionFor before send).
type SessionLookup interface {
	SessionFor(key string) *session.ManagedSession
	ListSessions() []session.SessionSnapshot
}

// SessionLifecycle is the create/recreate/remove sub-capability used by RPC
// handlers that allocate or tear down sessions.
type SessionLifecycle interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	ResetAndRecreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, error)
	Takeover(ctx context.Context, key string, sessionID string, workspace string, opts session.AgentOpts) (*session.ManagedSession, error)
	Remove(key string) bool
	DefaultWorkspace() string
}

// SessionMutator is the in-place mutation sub-capability (interrupt, label
// update): mutators preserve session identity while lifecycle ops swap or
// destroy the underlying *ManagedSession.
type SessionMutator interface {
	InterruptSessionSafe(key string) session.InterruptOutcome
	SetUserLabel(key, label string) bool
}

// SessionBackends is the read-only backend-manifest sub-capability used by the
// "fetch_backends" reverse-RPC branch; it assembles the same
// {backends, default, detected} payload GET /api/cli/backends serves locally.
type SessionBackends interface {
	BackendsManifest(detected []cli.BackendInfo) session.BackendManifest
}

// SessionRouter is the *Connector-only subset of *session.Router, composed
// from the four narrow sub-interfaces above.
type SessionRouter interface {
	SessionLookup
	SessionLifecycle
	SessionMutator
	SessionBackends
}
