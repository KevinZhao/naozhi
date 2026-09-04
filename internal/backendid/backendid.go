// Package backendid is the leaf package holding the per-request backend-ID
// length + charset gate shared by internal/server and internal/dashboard/cron
// (#1893).
package backendid

// MaxLen is the per-request backend-ID byte cap; it must match
// session/router_backend.go's maxBackendBytes (#1314).
const MaxLen = 64

// IsValid reports whether s passes the per-request charset + length gate.
// Empty is allowed (treated as "router default" by selectNodeForBackend).
// The charset is [A-Za-z0-9._-]; length must be <= MaxLen bytes.
func IsValid(s string) bool {
	if len(s) > MaxLen {
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
