// Package cron hosts the dashboard endpoints for cron job CRUD + run history
// + transcript. Phase 1 (server-split-phase4-design.md §6.5 Plan B) moved
// these from internal/server.
//
// Cross-cutting helpers below are accepted as interfaces (or pulled from leaf
// packages) rather than reverse-imported from internal/server, keeping the
// dependency direction one-way.
package cron

import (
	"github.com/naozhi/naozhi/internal/backendid"
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
)

// IPLimiter aliases the shared dashboard contract (#2285) so server's
// *ipLimiter is injected without a reverse import and the cron package keeps
// its local name for Deps fields and tests.
type IPLimiter = contracts.IPLimiter

// maxBackendIDLen / isValidBackendID alias the shared backendid leaf package's
// length+charset gate so this package and internal/server enforce one contract
// without a reverse import (R20260607-ARCH-2 #1893).
const maxBackendIDLen = backendid.MaxLen

func isValidBackendID(s string) bool { return backendid.IsValid(s) }
