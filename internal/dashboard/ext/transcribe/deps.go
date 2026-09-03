// Package transcribe hosts the dashboard /api/transcribe endpoint that
// converts dashboard-uploaded audio to text. Phase 3d
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package transcribe

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// IPLimiter aliases the shared dashboard contract (#2285). The multipart
// field cap previously duplicated here now lives in
// httputil.RejectIfTooManyFields / httputil.MaxMultipartFields.
type IPLimiter = contracts.IPLimiter
