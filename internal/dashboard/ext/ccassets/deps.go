// Package ccassets hosts the dashboard /api/cc/* endpoints that surface a
// backend's installed assets (skills/plugins/agents/...) read-only. It is a
// thin HTTP layer over assets.Provider implementations: parse params, rate
// limit, call Scan/ReadRaw, slice by kind, write JSON. It holds no on-disk
// layout knowledge (that lives in internal/ccassets) and does not reverse-
// import internal/server (deps injected as interfaces, mirroring ext/memory).
//
// RFC docs/rfc/cc-asset-browser.md §3.3.
package ccassets

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// IPLimiter aliases the shared dashboard contract (#2285); server's
// *ipLimiter is injected without a reverse import.
type IPLimiter = contracts.IPLimiter

// Rate-limit defaults for the asset endpoints. Asset listing is heavier than a
// memory hover (a cold scan walks many dirs) but still cheap once cached;
// these mirror the memory limiter's order of magnitude.
const (
	AssetsLimiterRate  = 10
	AssetsLimiterBurst = 20
)
