package auth

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/netutil"
)

// AuthCookieName is the dashboard auth cookie name. Exported so internal/server
// middleware can reference it without holding a private copy.
const AuthCookieName = "naozhi_auth"

// unknownIPKey is the shared bucket used when client IP resolution fails
// (duplicated locally to avoid reverse-importing internal/server).
const unknownIPKey = "_unknown_"

// requestHasResolvableClientIP reports whether r carries a usable per-client
// rate-limit key; delegates to netutil so the loopback-direct-access exemption
// is shared with internal/server.
func requestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	return netutil.RequestHasResolvableClientIP(r, trustedProxy)
}
