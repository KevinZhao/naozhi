package dispatch

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestResolveOrFabricateKeyResolver_Precedence locks in the single-track
// precedence chain that replaced the previous double-fallback inline
// branch in NewDispatcher (#543 R215-CR-P2-3). Each call must return a
// non-nil resolver, and an explicit cfg.Resolver must always win over
// the router-attached and fabricated paths.
func TestResolveOrFabricateKeyResolver_Precedence(t *testing.T) {
	t.Parallel()

	explicit := session.NewKeyResolver(nil, nil)

	// 1. cfg.Resolver wins.
	got := resolveOrFabricateKeyResolver(DispatcherConfig{Resolver: explicit})
	if got != explicit {
		t.Fatalf("explicit resolver not preferred: got %p want %p", got, explicit)
	}

	// 2. nil cfg.Resolver + nil Router falls through to fabrication —
	// must not return nil and must not panic with nil agents/projectmgr.
	got = resolveOrFabricateKeyResolver(DispatcherConfig{})
	if got == nil {
		t.Fatal("nil cfg + nil router: helper returned nil — fabrication path broken")
	}

	// 3. nil cfg.Resolver + Router with attached resolver adopts the
	// router-attached singleton.
	routerAttached := session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil)
	router := session.NewRouter(session.RouterConfig{Resolver: routerAttached})
	t.Cleanup(func() { router.Shutdown() })
	got = resolveOrFabricateKeyResolver(DispatcherConfig{Router: router})
	if got != routerAttached {
		t.Fatalf("router-attached resolver not adopted: got %p want %p", got, routerAttached)
	}

	// 4. Explicit Resolver beats router-attached.
	got = resolveOrFabricateKeyResolver(DispatcherConfig{Router: router, Resolver: explicit})
	if got != explicit {
		t.Fatalf("explicit resolver did not beat router-attached: got %p want %p (router %p)",
			got, explicit, routerAttached)
	}
}
