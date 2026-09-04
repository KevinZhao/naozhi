// Package api publishes the narrow capability interface(s) a consumer of
// *session.Router can embed instead of re-declaring its own subset.
// *session.Router satisfies them structurally; assert.go pins that so a
// Router signature change surfaces as a build failure here (#1600).
package api

import (
	"github.com/naozhi/naozhi/internal/session"
)

// SessionVisitor is the streaming read capability used by background
// daemons (sysession AutoTitler) that filter candidates without
// materialising a slice. fn returning false stops iteration early.
type SessionVisitor interface {
	VisitSessions(fn func(session.SessionSnapshot) bool)
}
