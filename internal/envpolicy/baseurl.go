// Package envpolicy holds the shared, leaf-level env-filtering primitives used
// by the Claude subprocess env policy: base-URL SSRF/redirect validation, AWS
// profile-name validation, and the per-backend raw-credential matrix.
//
// These were extracted (#891, RFC envpolicy-consolidation Phase 1) from
// internal/sysession and cmd/naozhi, which each carried byte-identical copies.
// The functions are pure (no side effects, no logging) — callers keep their own
// logging at the rejection site. Behaviour is unchanged from the originals.
package envpolicy

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateBaseURLValue enforces that an API base-URL passed through to a Claude
// subprocess uses https:// unless it targets a loopback host (localhost /
// 127.0.0.0/8 / ::1), for which plain http is allowed so operators can wire
// local mock gateways. An empty value is accepted (clears the var).
// R090031-SEC-1 (#1687) / R20260602-SEC-1 (#1576).
func ValidateBaseURLValue(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		host := u.Hostname()
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return fmt.Errorf("link-local host %q rejected (SSRF/IMDS guard)", host)
			}
		}
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() {
				return nil
			}
			if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return fmt.Errorf("link-local host %q rejected (SSRF/IMDS guard)", host)
			}
		}
		return fmt.Errorf("plain http:// to non-loopback host %q rejected (SSRF/redirect guard); use https://", host)
	}
	return fmt.Errorf("scheme %q not allowed; use https://", u.Scheme)
}
