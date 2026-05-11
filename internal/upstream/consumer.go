// Package upstream — consumer.go
//
// SessionRouter is the subset of *session.Router that Connector uses
// when translating primary-reverse RPC into local router operations.
// Declared here (not in session) so Connector tests can inject a fake
// without starting a real router; *session.Router satisfies the
// interface implicitly via Go structural typing, guarded at CI time
// by internal/session/contract_test.go.
//
// Method list = grep 'c\.router\.' internal/upstream/connector.go
// (12 call sites, 8 distinct methods) + constructor-time
// `router.DefaultWorkspace()` at connector.go:93. The constructor
// call is included so New's formal parameter can be the interface
// itself in a future refactor; today the caller still passes
// *session.Router and assigns to the interface field.
//
// Single interface per consumer (cron.SessionRouter precedent).
// 9 methods — no sub-aggregation needed at this size.
package upstream

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// SessionRouter is the *Connector-only subset of *session.Router.
type SessionRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	GetSession(key string) *session.ManagedSession
	ListSessions() []session.SessionSnapshot
	Remove(key string) bool
	ResetAndRecreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, error)
	Takeover(ctx context.Context, key string, sessionID string, workspace string, opts session.AgentOpts) (*session.ManagedSession, error)
	InterruptSessionSafe(key string) session.InterruptOutcome
	SetUserLabel(key, label string) bool
	DefaultWorkspace() string
}
