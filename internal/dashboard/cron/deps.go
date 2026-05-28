// Package cron hosts the dashboard endpoints for cron job CRUD + run history
// + transcript. Phase 1 (server-split-phase4-design.md §6.5 Plan B) moved
// these from internal/server.
//
// Cross-cutting helpers below are duplicated locally rather than reverse-
// imported from internal/server, keeping the dependency direction one-way.
package cron

import (
	"net/http"
)

// IPLimiter is the subset of internal/server.ipLimiter the cron handlers use.
// server's *ipLimiter satisfies this shape; we accept the interface so the
// sub-package doesn't reverse-import server.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// maxBackendIDLen / isValidBackendID duplicate internal/server/select_node_for_backend.go's
// length+charset gate so this package doesn't reverse-import server.
const maxBackendIDLen = 64

func isValidBackendID(s string) bool {
	if len(s) > maxBackendIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
