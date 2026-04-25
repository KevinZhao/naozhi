package session

import (
	"errors"
	"path/filepath"
	"strings"
)

// MaxRemoteWorkspacePath is the upper bound accepted by
// ValidateRemoteWorkspacePath. Matches the POSIX PATH_MAX on Linux and is
// well above any legitimate workspace depth.
const MaxRemoteWorkspacePath = 4096

// ValidateRemoteWorkspacePath performs the syntactic workspace checks that
// must fire before a path crosses a trust boundary and becomes the CWD of a
// spawned CLI process. It is intentionally conservative — it refuses any
// value that would be ambiguous after filepath.Clean (which silently folds
// `/home/../etc` to `/etc`) and any value containing control bytes that
// would corrupt log attrs or sessions.json storage.
//
// Callers:
//   - dashboard/HTTP layer via server.validateRemoteWorkspace (kept there
//     for backwards-compat with existing tests).
//   - upstream.Connector for reverse-RPC `send` / `takeover`, where the
//     defaultWorkspace prefix check used to be skipped entirely when
//     defaultWorkspace=="" (single-user deployments). With this gate,
//     a compromised primary can no longer inject arbitrary absolute paths
//     like `/etc` into a reverse node's CLI spawn even without an allowedRoot.
//     R68-SEC-M2.
//
// Empty input is treated as "use the caller's default" and passes — callers
// that require a workspace must check non-empty separately.
func ValidateRemoteWorkspacePath(workspace string) error {
	if workspace == "" {
		return nil
	}
	if len(workspace) > MaxRemoteWorkspacePath {
		return errors.New("workspace too long")
	}
	for i := 0; i < len(workspace); i++ {
		c := workspace[i]
		if c == 0 {
			return errors.New("invalid workspace")
		}
		if c < 0x20 || c == 0x7f {
			return errors.New("invalid workspace")
		}
	}
	if !filepath.IsAbs(workspace) {
		return errors.New("workspace must be absolute")
	}
	// Reject any literal `..` segment BEFORE filepath.Clean would fold it
	// into a now-canonical absolute path. Post-Clean checks would let
	// `/home/../etc` slip through as `/etc`.
	for _, seg := range strings.Split(workspace, string(filepath.Separator)) {
		if seg == ".." {
			return errors.New("workspace contains traversal segment")
		}
	}
	return nil
}
