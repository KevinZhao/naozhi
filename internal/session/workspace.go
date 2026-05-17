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
// spawned CLI process. It is intentionally conservative — it refuses any
// value that would be ambiguous after filepath.Clean (which silently folds
// `/home/../etc` to `/etc`) and any value containing control bytes or
// log-injection runes that would corrupt log attrs or sessions.json storage.
//
// Rejection rules (a path must satisfy ALL of these to pass):
//   - MUST be absolute (filepath.IsAbs).
//   - MUST NOT exceed MaxRemoteWorkspacePath (4096) bytes.
//   - MUST NOT contain a literal `..` path segment (rejected BEFORE
//     filepath.Clean so `/home/../etc` cannot silently resolve to `/etc`).
//   - MUST be valid UTF-8.
//   - MUST NOT contain any C0 control byte (< 0x20), DEL (0x7f), NUL, or
//     any rune that IsLogInjectionRune flags (C1 U+0080..U+009F, bidi
//     overrides U+202A..U+202E, bidi isolates U+2066..U+2069, LS/PS
//     U+2028/U+2029). Aligns with session.ValidateUserLabel so an attacker
//     cannot smuggle rendering-flipping characters through one trust
//     boundary that are rejected by the other.
//
// Empty input is treated as "use the caller's default" and passes — callers
// that require a workspace must check non-empty separately.
//
// Callers (add new call sites to this list so the single source of truth
// for the trust-boundary contract stays visible):
//   - dashboard/HTTP layer via server.validateRemoteWorkspace (kept there
//     for backwards-compat with existing tests).
//   - upstream.Connector for reverse-RPC `send` / `takeover` / `close_discovered`
//     CWD validation, where the defaultWorkspace prefix check used to be
//     skipped entirely when defaultWorkspace=="" (single-user deployments).
//     R68-SEC-M2, R70-SEC-MED, R71-DOC-L1.
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
		// C0 controls and DEL — NUL (0x00) is inside the `r < 0x20`
		// range and is covered here too (utf8.ValidString above has
		// already guaranteed every rune comes from a valid multi-byte
		// sequence, so there is no way to smuggle a bare 0x00 past
		// this check).
		if r < 0x20 || r == 0x7f {
			return errors.New("workspace contains C0 control byte")
		}
		// C1 / bidi override / bidi isolate / LS/PS — UTF-8-encoded
		// these slip past a byte-level scan entirely. Aligned with
		// session.ValidateUserLabel so one trust boundary cannot admit
		// characters the other would reject.
		if osutil.IsLogInjectionRune(r) {
			return errors.New("workspace contains bidi or C1 control rune")
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
