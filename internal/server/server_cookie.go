// loadOrCreateCookieSecret：启动期一次性 helper（symlink defence + 0600 perm
// gate + atomic write + 失败时 ephemeral fallback）。
package server

import (
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
)

// loadOrCreateCookieSecret reads a 32-byte secret from stateDir/cookie_secret,
// creating it with crypto/rand if absent. Falls back to a fresh ephemeral secret
// if the file cannot be read or written (e.g. no stateDir configured).
func loadOrCreateCookieSecret(stateDir string) []byte {
	if stateDir != "" {
		// A symlinked stateDir would let a local attacker serve their own
		// well-formed cookie_secret, bypassing the per-file symlink check
		// below; flag it and fall back to an ephemeral secret.
		if fi, err := os.Lstat(stateDir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			slog.Error("cookie_secret regenerated because stateDir is a symlink",
				"state_dir", stateDir, "reason", "statedir_symlink")
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				panic("crypto/rand unavailable: " + err.Error())
			}
			return b
		}
		path := filepath.Join(stateDir, "cookie_secret")
		// Lstat (not Stat) so a symlinked cookie_secret is rejected instead of
		// validated against the target's mode and leaking its contents via the
		// cookie secret.
		if fi, err := os.Lstat(path); err == nil {
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				slog.Error("cookie_secret regenerated because file is a symlink",
					"path", path, "reason", "symlink")
			case fi.Mode().Perm() != 0600:
				// Error-level with reason: rotation invalidates every browser session.
				slog.Error("cookie_secret regenerated due to unsafe permissions",
					"path", path, "mode", fi.Mode().Perm(), "reason", "unsafe_permissions")
			default:
				if data, err := os.ReadFile(path); err == nil && len(data) == 32 {
					return data
				}
			}
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic("crypto/rand unavailable: " + err.Error())
		}
		// Persistence is best-effort but failure must be Error-level: an
		// in-memory secret silently invalidates every browser session on each
		// restart, which looks like a token-expiry bug.
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			slog.Error("cookie_secret stateDir mkdir failed; session secret is ephemeral, all sessions will be invalidated on restart",
				"state_dir", stateDir, "err", err, "reason", "mkdir_failed")
		} else {
			// Atomic tmp+rename so a concurrent reader never sees a partial
			// secret during rotation.
			if err := osutil.WriteFileAtomic(path, b, 0600); err != nil {
				slog.Error("cookie_secret atomic write failed; session secret is ephemeral, all sessions will be invalidated on restart",
					"path", path, "err", err, "reason", "write_failed")
			}
		}
		return b
	}
	// No stateDir: ephemeral secret (sessions lost on restart)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}
