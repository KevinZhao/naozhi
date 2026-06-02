package api

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestRouterSatisfiesCapabilities is a runtime mirror of the compile-time
// assertions in assert.go. It documents, for a reader scanning the test
// suite, exactly which capability interfaces the concrete router fulfils.
func TestRouterSatisfiesCapabilities(t *testing.T) {
	var r *session.Router // nil is fine: we only assert assignability.

	if _, ok := interface{}(r).(SessionLookup); !ok {
		t.Error("*session.Router must satisfy SessionLookup")
	}
	if _, ok := interface{}(r).(SessionLifecycle); !ok {
		t.Error("*session.Router must satisfy SessionLifecycle")
	}
	if _, ok := interface{}(r).(SessionMutator); !ok {
		t.Error("*session.Router must satisfy SessionMutator")
	}
	if _, ok := interface{}(r).(SessionVisitor); !ok {
		t.Error("*session.Router must satisfy SessionVisitor")
	}
	if _, ok := interface{}(r).(SessionRouter); !ok {
		t.Error("*session.Router must satisfy SessionRouter union")
	}
}

// fakeLookup proves a consumer can implement a single narrow mixin
// without depending on the whole router surface — the core win of the
// split (R246-ARCH-11 / #791).
type fakeLookup struct{}

func (fakeLookup) GetSession(string) *session.ManagedSession { return nil }
func (fakeLookup) ListSessions() []session.SessionSnapshot   { return nil }

func TestNarrowMixinIsInjectable(t *testing.T) {
	var _ SessionLookup = fakeLookup{}
}
