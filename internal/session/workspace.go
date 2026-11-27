package session

import "github.com/naozhi/naozhi/internal/session/sessionview"

// ValidateRemoteWorkspacePath performs the syntactic workspace checks a path
// must pass before it crosses a trust boundary; see
// sessionview.ValidateRemoteWorkspacePath.
func ValidateRemoteWorkspacePath(workspace string) error {
	return sessionview.ValidateRemoteWorkspacePath(workspace)
}
