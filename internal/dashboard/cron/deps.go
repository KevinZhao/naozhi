// Package cron hosts the dashboard endpoints for cron job CRUD + run history
// + transcript. Phase 1 (server-split-phase4-design.md §6.5 Plan B) moved
// these from internal/server.
//
// Cross-cutting helpers below are accepted as interfaces (or pulled from leaf
// packages) rather than reverse-imported from internal/server, keeping the
// dependency direction one-way.
package cron

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/backendid"
)

// IPLimiter is the subset of internal/server.ipLimiter the cron handlers use.
// server's *ipLimiter satisfies this shape; we accept the interface so the
// sub-package doesn't reverse-import server.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// maxBackendIDLen / isValidBackendID alias the shared backendid leaf package's
// length+charset gate so this package and internal/server enforce one contract
// without a reverse import (R20260607-ARCH-2 #1893).
const maxBackendIDLen = backendid.MaxLen

func isValidBackendID(s string) bool { return backendid.IsValid(s) }
