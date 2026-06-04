package api

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestRouterSatisfiesCapabilities is a runtime mirror of the compile-time
// assertion in assert.go. After #1600 only SessionVisitor remains (the
// other mixins were removed as zero-consumer dead code).
func TestRouterSatisfiesCapabilities(t *testing.T) {
	var r *session.Router // nil is fine: we only assert assignability.

	if _, ok := interface{}(r).(SessionVisitor); !ok {
		t.Error("*session.Router must satisfy SessionVisitor")
	}
}

// fakeVisitor proves a consumer can implement the narrow SessionVisitor
// mixin without depending on the whole router surface — the core win of
// the split that survived #1600 (the sysession AutoTitler consumer).
type fakeVisitor struct{}

func (fakeVisitor) VisitSessions(func(session.SessionSnapshot) bool) {}

func TestNarrowMixinIsInjectable(t *testing.T) {
	var _ SessionVisitor = fakeVisitor{}
}
