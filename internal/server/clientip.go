package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/netutil"
)

// clientIP is a thin wrapper around netutil.ClientIP kept so existing
// server-package call sites can keep their short name. See the netutil
// docs for semantics (trusted-proxy XFF last-hop, IP validation).
func clientIP(r *http.Request, trustedProxy bool) string {
	return netutil.ClientIP(r, trustedProxy)
}
