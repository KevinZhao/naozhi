// Package ccassets hosts the dashboard /api/cc/* endpoints that surface a
// backend's installed assets (skills/plugins/agents/...) read-only. It is a
// thin HTTP layer over assets.Provider implementations: parse params, rate
// limit, call Scan/ReadRaw, slice by kind, write JSON. It holds no on-disk
// layout knowledge (that lives in internal/ccassets) and does not reverse-
// import internal/server (deps injected as interfaces, mirroring ext/memory).
//
// RFC docs/rfc/cc-asset-browser.md §3.3.
package ccassets

import "net/http"

// IPLimiter is the subset of internal/server.ipLimiter this handler uses.
// server's *ipLimiter satisfies this shape; accepting the interface keeps the
// dependency one-way (mirrors ext/memory/deps.go).
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// Rate-limit defaults for the asset endpoints. Asset listing is heavier than a
// memory hover (a cold scan walks many dirs) but still cheap once cached;
// these mirror the memory limiter's order of magnitude.
const (
	AssetsLimiterRate  = 10
	AssetsLimiterBurst = 20
)
