// Package api publishes the narrow capability interfaces that consumers
// of *session.Router compose ad-hoc, instead of each consumer package
// re-declaring its own "subset of session.Router" interface.
//
// Background (R243-ARCH-9 / R245-ARCH-35 / R246-ARCH-11 / R240-ARCH-15,
// issues #791 / #1032): four+ consumer packages (upstream, dispatch,
// cron, server/wshub, sysession) each declared an independent
// SessionRouter interface (9 / 8 / 3 / 14 / 4 methods). Signatures drift
// independently and every shared method change forces a synchronised
// edit across all the contract tests.
//
// The upstream package already split its interface into Lookup /
// Lifecycle / Mutator sub-interfaces as a forward-compatible step. This
// package hoists exactly that shape to a shared location so each consumer
// can embed only the mixins it needs:
//
//	type myRouter interface {
//	    api.SessionLookup
//	    api.SessionMutator
//	}
//
// *session.Router satisfies every interface here implicitly via Go
// structural typing; the compile-time assertions in assert.go pin that
// guarantee so a method-signature change on Router that breaks a consumer
// surfaces as a build failure in this one package rather than as silent
// drift across five.
//
// Migration is intentionally staged: this package lands first (no
// behaviour, no consumer edits). Consumers then switch to embedding the
// api mixins in their own commits, each keeping its own contract test
// until the last consumer migrates.
package api

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// SessionLookup is the read-only lookup sub-capability used by hot paths
// (subscribe stream filter, ListSessions response, GetSession before
// send). Pure read access — no allocation of new sessions, no mutation
// of existing state.
type SessionLookup interface {
	GetSession(key string) *session.ManagedSession
	ListSessions() []session.SessionSnapshot
}

// SessionLifecycle is the create/recreate/remove/takeover sub-capability
// used by handlers that allocate or tear down sessions. Lifecycle ops
// may swap or destroy the underlying *ManagedSession.
type SessionLifecycle interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	ResetAndRecreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, error)
	Takeover(ctx context.Context, key string, sessionID string, workspace string, opts session.AgentOpts) (*session.ManagedSession, error)
	Remove(key string) bool
	DefaultWorkspace() string
}

// SessionMutator is the in-place mutation sub-capability used by
// operations that touch live sessions (interrupt, label update). Mutators
// preserve session identity, unlike SessionLifecycle ops.
type SessionMutator interface {
	InterruptSessionSafe(key string) session.InterruptOutcome
	SetUserLabel(key, label string) bool
}

// SessionVisitor is the streaming read sub-capability used by background
// daemons (sysession AutoTitler) that filter candidates without
// materialising a slice. fn returning false stops iteration early.
type SessionVisitor interface {
	VisitSessions(fn func(session.SessionSnapshot) bool)
}

// SessionRouter is the union of all capability mixins above — equivalent
// to the broadest consumer subset of *session.Router. Consumers should
// prefer embedding the narrowest set of mixins they actually use; this
// union exists for callers that genuinely need full read+lifecycle+mutate
// access.
type SessionRouter interface {
	SessionLookup
	SessionLifecycle
	SessionMutator
}
