package session

import (
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/session/backendstore"
)

// BackendRegistry is Router's backend facet: which backends exist, which
// wrapper and defaults each resolves to, and the named access profiles a
// spawn may overlay. None of it touches the session table, so it holds no
// Router pointer and takes no table lock. Reach it through Router.Backends;
// the zero value (a hand-built test Router) reads as empty.
type BackendRegistry struct {
	// bk is the backend table (internal/session/backendstore), fixed once
	// NewRouter returns; nil on a hand-built test Router, which reads as empty.
	bk *backendstore.Store
	// accessProfiles is the named auth/upstream overlay registry (RFC
	// project-access-profile). Nil/empty ⇒ every session runs on the global
	// baseline. Copy-on-write behind an atomic pointer: AddAccessProfile
	// publishes a whole new map, so readers load it without the table lock and never see
	// a half-inserted entry. Read it through profiles().
	accessProfiles atomic.Pointer[map[string]AccessProfile]
	// defaultAccessProfile is applied when a session resolves to no explicit
	// profile (lowest precedence); "" = global-baseline fallthrough. Read-only after NewRouter.
	defaultAccessProfile string
}

// Backends returns the router's backend facet; nil for a nil Router.
func (r *Router) Backends() *BackendRegistry {
	if r == nil {
		return nil
	}
	return &r.backends
}
