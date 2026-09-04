// Package ccassets hosts the read-only dashboard /api/cc/* asset-browser
// endpoints: a thin HTTP layer over assets.Provider (parse params, rate
// limit, Scan/ReadRaw, write JSON). RFC docs/rfc/cc-asset-browser.md §3.3.
package ccassets

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// IPLimiter aliases the shared dashboard contract (#2285).
type IPLimiter = contracts.IPLimiter

// Rate-limit defaults for the asset endpoints (mirror the memory limiter).
const (
	AssetsLimiterRate  = 10
	AssetsLimiterBurst = 20
)
