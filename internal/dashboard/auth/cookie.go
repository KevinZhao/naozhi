package auth

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/netutil"
)

// AuthCookieName is the dashboard auth cookie name.
//
// Phase 3a (server-split-phase4-design.md §6.5 Plan B): exported here so the
// internal/server package can reference the const without holding a private
// copy. Cookie issue/verify lives in this package; server-package middleware
// that just reads the cookie name imports this constant.
const AuthCookieName = "naozhi_auth"

// unknownIPKey is the shared bucket used when client IP resolution fails.
// Phase 3a: duplicated locally so this package doesn't reverse-import
// internal/server's ip_limiter.go private const.
const unknownIPKey = "_unknown_"

// requestHasResolvableClientIP reports whether r carries a usable per-client
// rate-limit key. Delegates to netutil so the loopback-direct-access
// exemption (R20260605) is shared with internal/server: in trustedProxy mode
// an XFF-carrying request OR a loopback direct connection (SSH tunnel / local
// curl) is resolvable, while an externally-routable XFF-less request stays
// unresolvable.
func requestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	return netutil.RequestHasResolvableClientIP(r, trustedProxy)
}
