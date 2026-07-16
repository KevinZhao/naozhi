// Package accessprofile hosts the dashboard /api/access-profiles
// endpoints (RFC project-access-profile §8.1 + P1-d): the read-only
// registry listing and the runtime create flow.
//
// Moved from internal/server (routes.go + access_profile_create.go) per
// lint rule 1 (server-split-phase4-design.md §9.2: no new *Server
// handle* methods after Phase 0; unplanned violations move to a
// dashboard sub-package).
package accessprofile

import (
	"github.com/naozhi/naozhi/internal/session"
)

// Router is the subset of *session.Router the access-profile handlers
// use. Consumer-side interface (same shape as agentevents.NodeAccessor)
// so the sub-package doesn't import internal/server or depend on the
// concrete router beyond these five methods.
type Router interface {
	// AccessProfileInfos projects the registry down to non-sensitive
	// display fields + a secret_ok preflight bit. Env values and *_FILE
	// contents never cross this boundary.
	AccessProfileInfos() []session.AccessProfileInfo
	// DefaultAccessProfile returns the configured default profile ID
	// ("" when none), used to pre-select the new-session picker.
	DefaultAccessProfile() string
	// HasAccessProfile reports whether id is already registered.
	HasAccessProfile(id string) bool
	// AddAccessProfile registers a profile in the live registry so a
	// freshly-created profile works without a restart.
	AddAccessProfile(id string, p session.AccessProfile) error
	// BackendIDs lists the enabled backend IDs (default_backend guard).
	BackendIDs() []string
}
