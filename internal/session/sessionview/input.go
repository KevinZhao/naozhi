package sessionview

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

// MaxUserLabelBytes caps the operator-set session label. 128 bytes covers any
// realistic sidebar/header title while keeping sessions.json growth bounded —
// the label is rebroadcast on every /api/sessions poll, so a megabyte-scale
// string would multiply dashboard egress N×(tabs).
const MaxUserLabelBytes = 128

// ValidateUserLabel trims surrounding whitespace, enforces MaxUserLabelBytes,
// rejects invalid UTF-8, and blocks ASCII / C1 control characters that would
// otherwise corrupt slog JSONHandler output, terminal log viewers, or
// dashboard HTML. An empty return value is the caller's signal to clear any
// prior label.
//
// Shared by internal/server (dashboard HTTP path) and internal/upstream
// (reverse-RPC worker) so both trust boundaries apply identical rules. The
// upstream path is load-bearing against a compromised control-node.
func ValidateUserLabel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return "", nil
	}
	if len(s) > MaxUserLabelBytes {
		return "", fmt.Errorf("label exceeds %d-byte limit", MaxUserLabelBytes)
	}
	if !utf8.ValidString(s) {
		return "", errors.New("invalid utf-8")
	}
	for _, r := range s {
		// Reject C0, DEL, C1 control ranges plus bidi overrides/isolates and
		// LS/PS (IsLogInjectionRune), which could flip the rendered sidebar
		// order. Tab is NOT exempted: slog.TextHandler uses tab as its
		// key/value separator, so a tab in a label would fragment log output.
		if r == 0 || r < 0x20 || (r >= 0x7F && r <= 0x9F) || osutil.IsLogInjectionRune(r) {
			return "", errors.New("control characters not allowed")
		}
	}
	return s, nil
}
