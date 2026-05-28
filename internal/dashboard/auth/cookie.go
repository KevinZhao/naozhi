package auth

import (
	"net"
	"net/http"
	"strings"
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
// rate-limit key. In !trustedProxy mode every request has a key (RemoteAddr
// is set by net/http even for UDS via the listener fallback). In trustedProxy
// mode the request is only resolvable when X-Forwarded-For carries at least
// one parseable IP.
//
// Phase 3a duplicated from internal/server/ip_limiter.go.
func requestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	if !trustedProxy {
		return true
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return false
	}
	tail := xff
	if i := strings.LastIndexByte(xff, ','); i >= 0 {
		tail = xff[i+1:]
	}
	tail = strings.TrimSpace(tail)
	return tail != "" && net.ParseIP(tail) != nil
}
