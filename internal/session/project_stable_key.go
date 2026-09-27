package session

import "github.com/naozhi/naozhi/internal/sessionkey"

// ProjectStableKey returns the canonical project-level stable dashboard
// session key for a workspace; see sessionkey.ProjectStableKey.
func ProjectStableKey(absPath, agent string) string {
	return sessionkey.ProjectStableKey(absPath, agent)
}
