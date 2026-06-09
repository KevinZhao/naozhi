// Package backendid centralizes the per-request backend-ID length + charset
// gate shared by the HTTP send path (internal/server), the WS handlers
// (internal/server, internal/wshub), and the dashboard cron CRUD endpoints
// (internal/dashboard/cron).
//
// Before R20260607-ARCH-2 (#1893) the identical const + validator was copied
// into internal/server/select_node_for_backend.go and
// internal/dashboard/cron/deps.go to avoid a reverse import of internal/server
// from the dashboard sub-package. This leaf package — which imports nothing
// from the project — lets both sides share one definition while keeping the
// dependency direction one-way.
package backendid

// MaxLen is the per-request backend-ID byte cap.
//
// R20260527122801-ARCH-8 (#1314): aligned to 64 to match
// session/router_backend.go's maxBackendBytes. The previous 32-byte server cap
// rejected legal 33–64 byte backend IDs at the dashboard / HTTP-send boundary
// even though the router's own validateBackend (charset+length) accepted them.
// Widening to 64 closes that asymmetry; the DoS concern motivating the smaller
// cap is unchanged at 64 bytes (still 1/64 of the 4 KB JSON-attr worst case).
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
