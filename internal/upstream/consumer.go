// Package upstream — consumer.go
//
// SessionRouter is the subset of *session.Router that Connector uses
// when translating primary-reverse RPC into local router operations.
// Declared here (not in session) so Connector tests can inject a fake
// without starting a real router; *session.Router satisfies the
// interface implicitly via Go structural typing, guarded at CI time
// by internal/session/contract_test.go.
//
// Method list = grep 'c\.router\.' internal/upstream/connector*.go
// (12 call sites, 8 distinct methods) + constructor-time
// `router.DefaultWorkspace()` at connector.go:105. The constructor
// call is included so New's formal parameter can be the interface
// itself in a future refactor; today the caller still passes
// *session.Router and assigns to the interface field.
//
// NEEDS-DESIGN (R243-ARCH-9, REPEAT-9): four consumer-side
// SessionRouter interfaces co-exist across {upstream, dispatch, cron,
// server} (9 / 8 / 3 / 14 methods each). The long-term plan is to
// hoist a single `internal/session/api/` sub-package that publishes
// narrow capability interfaces (Lookup / Lifecycle / Mutator / Stats
// / Subscribe) which each consumer composes ad-hoc — avoiding both
// the present duplication and the over-broad god-interface that a
// single shared SessionRouter type would become. The cross-package
// migration must be staged carefully because contract_test.go pins
// the exact method sets, dispatch's interface participates in
// Capability injection, and server's 14-method interface is the
// largest blast radius. Doing all four in one CL would risk a
// stale-test merge race; each consumer migrates in its own commit
// after the api/ sub-package lands.
//
// As a forward-compatible step we already split the upstream
// interface into three narrow sub-interfaces (SessionLookup,
// SessionLifecycle, SessionMutator) that compose into SessionRouter
// here, mirroring the eventual api/ shape. Consumer code may
// gradually depend on the narrowest sub-interface it actually needs;
// `*session.Router` continues to satisfy the union via Go structural
// typing. When api/ lands, these aliases either move there
// unchanged or get replaced by api.X with a `var _ api.X = ...`
// compile-time check in this file's place. Either path leaves
// callers untouched.
//
// Sizing constraint: keep this file ≤80 LOC of behaviour (helpers
// excluded) per the standing refactor budget so the eventual cross-
// package consolidation does not pull large amounts of behaviour
// out of a moved-then-rewritten file. The split below is interface
// declarations only — zero runtime helpers — so it is comfortably
// inside that budget.
package upstream

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
)

// SessionLookup is the read-only lookup sub-capability used by hot
// RPC paths (subscribe stream filter, ListSessions response, SessionFor
// before send). Pure read access — no allocation of new sessions, no
// mutation of existing state. A future api/ sub-package would expose
// this name verbatim.
type SessionLookup interface {
	SessionFor(key string) *session.ManagedSession
	ListSessions() []session.SessionSnapshot
}

// SessionLifecycle is the create/recreate/remove sub-capability used
// by RPC handlers that allocate or tear down sessions. Splitting this
// out lets future tests substitute a strict no-mutation fake when
// asserting that a handler must NOT mutate session lifecycle (e.g.
// pure-read RPCs that landed in the wrong handler table by mistake).
type SessionLifecycle interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	ResetAndRecreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, error)
	Takeover(ctx context.Context, key string, sessionID string, workspace string, opts session.AgentOpts) (*session.ManagedSession, error)
	Remove(key string) bool
	DefaultWorkspace() string
}

// SessionMutator is the in-place mutation sub-capability used by
// dashboard-equivalent RPC operations that touch live sessions
// (interrupt, label update). Kept separate from SessionLifecycle
// because mutators preserve identity while lifecycle ops swap or
// destroy the underlying *ManagedSession.
type SessionMutator interface {
	InterruptSessionSafe(key string) session.InterruptOutcome
	SetUserLabel(key, label string) bool
}

// SessionRouter is the *Connector-only subset of *session.Router. It
// composes the three narrow sub-interfaces above; equivalent to the
// flat 9-method form previously declared inline. Existing callers
// (and contract_test.go's compile-time assertion) continue to work
// unchanged because Go embedded-interface composition produces the
// same method set.
type SessionRouter interface {
	SessionLookup
	SessionLifecycle
	SessionMutator
}
