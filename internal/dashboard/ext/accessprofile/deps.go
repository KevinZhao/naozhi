// Package accessprofile hosts the dashboard /api/access-profiles endpoints:
// the read-only registry listing and the runtime create flow.
package accessprofile

import (
	"github.com/naozhi/naozhi/internal/session"
)

// Router is the consumer-side subset of *session.Router the access-profile
// handlers use, so the sub-package never imports internal/server.
type Router interface {
	// AccessProfileInfos projects the registry down to non-sensitive display
	// fields + a secret_ok bit; env values and *_FILE contents never cross.
	AccessProfileInfos() []session.AccessProfileInfo
	// DefaultAccessProfile returns the configured default profile ID ("" when none).
	DefaultAccessProfile() string
	HasAccessProfile(id string) bool
	// AddAccessProfile registers a profile in the live registry (no restart needed).
	AddAccessProfile(id string, p session.AccessProfile) error
	// BackendIDs lists the enabled backend IDs (default_backend guard).
	BackendIDs() []string
}
