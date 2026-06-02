// Phase 5-prep / R-server-cookie-extract (2026-05-28):
// loadOrCreateCookieSecret 抽到独立文件。纯物理切分、零行为变化。
//
// 这个 bootstrap-time helper 只在 buildServer 启动期被调用一次，逻辑
// 高度自包含（symlink defence + 0600 perm gate + atomic write + 失败
// 时 ephemeral fallback）；与 Server lifecycle 主流程无运行期耦合。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// randomCookieGen returns 16 bytes of CSPRNG entropy hex-encoded, used as the
// per-construction seed for the auth-cookie generation marker mixed into the
// cookie HMAC. R217-SEC-6 / R172-SEC-L4 (#595 / #437): an unpredictable seed
// ensures a captured cookie cannot be replayed against a future process that
// shares the same dashboard token + cookie secret. On the (practically
// impossible) rand.Read failure we fall back to a time-derived value so the
// process still starts — strictly no worse than the previous always-time seed.
func randomCookieGen() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}

// loadOrCreateCookieSecret reads a 32-byte secret from stateDir/cookie_secret,
// creating it with crypto/rand if absent. Falls back to a fresh ephemeral secret
// if the file cannot be read or written (e.g. no stateDir configured).
func loadOrCreateCookieSecret(stateDir string) []byte {
	if stateDir != "" {
		// Defence in depth: the symlink check below pins cookie_secret
		// itself, but a local attacker who can repoint stateDir (e.g.
		// stateDir → /tmp/pwn/ because the parent is world-writable)
		// bypasses that by placing a well-formed cookie_secret inside
		// their own directory. Lstat'ing stateDir first makes that
		// class of attack visible — a symlink'd stateDir gets flagged
		// and the secret is regenerated (ephemeral fallback) instead
		// of silently trusting whatever the target directory serves.
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
		// R188-SEC-L4: use os.Lstat so a symlink attack (e.g. cookie_secret →
		// /etc/some-readable-file) is detected instead of silently validated
		// against the target's mode. A local attacker who can write stateDir
		// would otherwise trick the check into passing against an arbitrary
		// file and leak its contents via the cookie secret ABI, or trigger
		// rotation loops that invalidate all sessions.
		if fi, err := os.Lstat(path); err == nil {
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				slog.Error("cookie_secret regenerated because file is a symlink",
					"path", path, "reason", "symlink")
			case fi.Mode().Perm() != 0600:
				// Log at Error with an explicit reason so monitoring can
				// distinguish "unsafe perms forced rotation" from first-run
				// regeneration. All existing browser sessions will be
				// invalidated — operator should know why.
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
		// R236-SEC-10: persistence is best-effort, but failure must surface
		// at Error level (not Warn). When the secret cannot be persisted the
		// process keeps running with an in-memory secret — the side effect
		// is that every restart silently invalidates every browser session,
		// which operators will mistake for a token expiry bug rather than a
		// disk / permissions misconfiguration. Error-level lines + an
		// explicit reason make the failure mode greppable in logs.
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			slog.Error("cookie_secret stateDir mkdir failed; session secret is ephemeral, all sessions will be invalidated on restart",
				"state_dir", stateDir, "err", err, "reason", "mkdir_failed")
		} else {
			// Write atomically (tmp + rename) so a concurrent reader never
			// sees a partial secret during rotation. os.WriteFile opens with
			// O_TRUNC and the crypto/rand bytes land in small chunks — a
			// parallel open+read could pick up N bytes of zeros if we were
			// mid-Write. WriteFileAtomic also fsyncs the file + parent dir.
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
