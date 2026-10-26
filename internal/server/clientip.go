package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/netutil"
)

// clientIP is a thin wrapper around netutil.ClientIP (trusted-proxy XFF
// last-hop, IP validation).
func clientIP(r *http.Request, trustedProxy bool) string {
	return netutil.ClientIP(r, trustedProxy)
}
