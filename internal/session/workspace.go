package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MaxRemoteWorkspacePath is the upper bound accepted by
// ValidateRemoteWorkspacePath. Matches the POSIX PATH_MAX on Linux and is
// well above any legitimate workspace depth.
const MaxRemoteWorkspacePath = 4096

// ValidateRemoteWorkspacePath performs the syntactic workspace checks that
// must fire before a path crosses a trust boundary and becomes the CWD of a
// spawned CLI process. A path must be absolute, ≤ MaxRemoteWorkspacePath bytes,
// valid UTF-8, free of literal `..` segments (checked BEFORE filepath.Clean,
// which silently folds `/home/../etc` to `/etc`), and free of C0/DEL control
// bytes and IsLogInjectionRune runes (C1, bidi overrides/isolates, LS/PS) —
// aligned with ValidateUserLabel so neither trust boundary admits characters
// the other rejects. Empty input means "use the caller's default" and passes.
// Callers: server.validateRemoteWorkspace and upstream.Connector (reverse-RPC).
func ValidateRemoteWorkspacePath(workspace string) error {
	if workspace == "" {
		return nil
	}
	if len(workspace) > MaxRemoteWorkspacePath {
		return fmt.Errorf("workspace exceeds %d-byte limit", MaxRemoteWorkspacePath)
	}
	if !utf8.ValidString(workspace) {
		return errors.New("workspace is not valid UTF-8")
	}
	for _, r := range workspace {
		// C0 controls (incl. NUL) and DEL. utf8.ValidString above guarantees
		// every rune comes from a valid sequence, so no bare 0x00 slips past.
		if r < 0x20 || r == 0x7f {
			return errors.New("workspace contains C0 control byte")
		}
		// C1 / bidi override / bidi isolate / LS/PS — UTF-8-encoded these
		// slip past a byte-level scan entirely.
		if osutil.IsLogInjectionRune(r) {
			return errors.New("workspace contains bidi or C1 control rune")
		}
	}
	if !filepath.IsAbs(workspace) {
		return errors.New("workspace must be absolute")
	}
	// Reject literal `..` BEFORE filepath.Clean would fold `/home/../etc`
	// into `/etc`.
	for _, seg := range strings.Split(workspace, string(filepath.Separator)) {
		if seg == ".." {
			return errors.New("workspace contains traversal segment")
		}
	}
	return nil
}
