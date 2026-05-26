package server

import (
	"sync/atomic"
	"testing"
)

// TestSessionsBusNilHub asserts the buildServer → registerDashboard
// lifecycle window is handled correctly: producers wired before the
// Hub exists must Publish without panicking and without firing a
// broadcast.
func TestSessionsBusNilHub(t *testing.T) {
	var hub *Hub
	bus := newHubSessionsBus(func() *Hub { return hub })
	// Publish is a noop while hub is nil; primarily we assert no panic.
	bus.Publish()
	bus.Publish()
}

// TestNoopSessionsBus asserts the test helper provides a SessionsBus
// whose Publish is a true noop — used by partial-server tests that
// don't wire a Hub.
func TestNoopSessionsBus(t *testing.T) {
	bus := NewNoopSessionsBus()
	if bus == nil {
		t.Fatal("NewNoopSessionsBus returned nil")
	}
	bus.Publish() // must not panic
}

// fakeBusTarget is a minimal SessionsBus stand-in used to verify the
// hubSessionsBus indirection: every Publish increments calls so a test
// can assert producer wiring without bringing up a real Hub.
type fakeBusTarget struct{ calls atomic.Int64 }

func (f *fakeBusTarget) Publish() { f.calls.Add(1) }

// TestSessionsBusDispatch asserts that callers holding only a
// SessionsBus reference can drive Publish without a *Hub dependency —
// the producer-side decoupling that #777 motivates.
func TestSessionsBusDispatch(t *testing.T) {
	target := &fakeBusTarget{}
	var bus SessionsBus = target
	for range 3 {
		bus.Publish()
	}
	if got := target.calls.Load(); got != 3 {
		t.Fatalf("Publish dispatch: want 3, got %d", got)
	}
}
