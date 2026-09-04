// Package transcribe hosts the dashboard /api/transcribe endpoint that
// converts dashboard-uploaded audio to text.
package transcribe

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// IPLimiter aliases the shared dashboard contract (#2285).
type IPLimiter = contracts.IPLimiter
