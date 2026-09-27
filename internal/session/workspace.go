package session

import "github.com/naozhi/naozhi/internal/session/sessionview"

// MaxRemoteWorkspacePath is the upper bound accepted by
// ValidateRemoteWorkspacePath; see sessionview.MaxRemoteWorkspacePath.
const MaxRemoteWorkspacePath = sessionview.MaxRemoteWorkspacePath

// ValidateRemoteWorkspacePath performs the syntactic workspace checks a path
// must pass before it crosses a trust boundary; see
// sessionview.ValidateRemoteWorkspacePath.
func ValidateRemoteWorkspacePath(workspace string) error {
	return sessionview.ValidateRemoteWorkspacePath(workspace)
}
