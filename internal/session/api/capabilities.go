// Package api publishes the narrow capability interface(s) that a
// consumer of *session.Router can embed instead of re-declaring its own
// "subset of session.Router" interface.
//
// Background (R243-ARCH-9 / R245-ARCH-35 / R246-ARCH-11 / R240-ARCH-15,
// issues #791 / #1032): several consumer packages each declared an
// independent SessionRouter-shaped interface. The intent of this package
// was to hoist shared mixins (Lookup / Lifecycle / Mutator / a Router
// union) so consumers could embed only what they need.
//
// Outcome (R164029-ARCH-1, #1600): that staged migration never landed.
// For over a year only SessionVisitor gained a real consumer (sysession
// router + router_adapter); SessionLookup / SessionLifecycle /
// SessionMutator / the SessionRouter union had ZERO consumers repo-wide
// while dispatch / cron / server kept their hand-written subsets. The
// unused mixins paid the abstraction cost with no benefit and the
// compile-time pins gave a false "already integrated" impression. Rather
// than carry dead-on-arrival interfaces indefinitely, they are removed
// here (the #1600 "plan B" resolution); SessionVisitor — the one mixin
// with a live consumer — stays, pinned by assert.go.
//
// *session.Router satisfies SessionVisitor implicitly via Go structural
// typing; the compile-time assertion in assert.go pins that guarantee so
// a method-signature change on Router that breaks the sysession consumer
// surfaces as a build failure in this one package.
package api

import (
	"github.com/naozhi/naozhi/internal/session"
)

// SessionVisitor is the streaming read sub-capability used by background
// daemons (sysession AutoTitler) that filter candidates without
// materialising a slice. fn returning false stops iteration early.
//
// This is the only capability mixin in this package with a live consumer
// (internal/sysession). The former SessionLookup / SessionLifecycle /
// SessionMutator / SessionRouter union were removed in #1600 after a
// year of zero adoption.
type SessionVisitor interface {
	VisitSessions(fn func(session.SessionSnapshot) bool)
}
