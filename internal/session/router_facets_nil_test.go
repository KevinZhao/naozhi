package session

import (
	"context"
	"testing"
)

// TestRouterFacets_NilRouter: a facet accessor on a nil Router returns nil
// rather than panicking. The accessors are nil-safe; the registry's methods
// still dereference it, as (*Router)(nil).CLIPath() did before the move. The
// two health reads were nil-safe on Router and stay so on a nil HistoryIO.
func TestRouterFacets_NilRouter(t *testing.T) {
	var r *Router
	if got := r.Backends(); got != nil {
		t.Errorf("(*Router)(nil).Backends() = %p, want nil", got)
	}
	if got := r.History(); got != nil {
		t.Errorf("(*Router)(nil).History() = %p, want nil", got)
	}
	if got := r.History().EventLogStats(); got != (EventLogHealth{}) {
		t.Errorf("(*Router)(nil).History().EventLogStats() = %+v, want the zero (disabled) value", got)
	}
	if got := r.History().AttachmentTrackerStats(); got != (AttachmentTrackerHealth{}) {
		t.Errorf("(*Router)(nil).History().AttachmentTrackerStats() = %+v, want the zero (disabled) value", got)
	}
}

// TestRouterFacets_AccessorIsTheRoutersOwn: Backends() hands out the router's
// own registry, not a copy, so a profile added through it is the one the
// spawn path resolves.
func TestRouterFacets_AccessorIsTheRoutersOwn(t *testing.T) {
	r := &Router{}
	if got := r.Backends(); got != &r.backends {
		t.Fatalf("Backends() = %p, want the router's field %p", got, &r.backends)
	}
	if err := r.Backends().AddAccessProfile("p1", AccessProfile{DefaultModel: "m1"}); err != nil {
		t.Fatal(err)
	}
	if got := r.backends.accessProfileDefaultModel("p1"); got != "m1" {
		t.Errorf("profile added through Backends() resolves to default model %q, want m1", got)
	}
}

// TestRouterFacets_HistoryIsTheRoutersOwn: History() hands out the router's
// own facet, not a copy, so a task it runs is one the router's Shutdown waits
// for and cancels.
func TestRouterFacets_HistoryIsTheRoutersOwn(t *testing.T) {
	r := NewRouter(RouterConfig{})
	if got := r.History(); got != &r.hist {
		t.Fatalf("History() = %p, want the router's field %p", got, &r.hist)
	}
	released := make(chan struct{})
	if !r.History().runHistoryTask(func(ctx context.Context) {
		<-ctx.Done()
		close(released)
	}) {
		t.Fatal("runHistoryTask refused a task on a live router")
	}
	r.Shutdown()
	select {
	case <-released:
	default:
		t.Error("Shutdown returned before the task it should have cancelled and waited for")
	}
}
