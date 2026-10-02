package session

import "testing"

// TestRouterFacets_NilRouter: a facet accessor on a nil Router returns nil
// rather than panicking. Only the accessor is nil-safe: the registry's methods
// still dereference it, as (*Router)(nil).CLIPath() did before the move.
func TestRouterFacets_NilRouter(t *testing.T) {
	var r *Router
	if got := r.Backends(); got != nil {
		t.Errorf("(*Router)(nil).Backends() = %p, want nil", got)
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
