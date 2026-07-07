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
	"os"
	"strings"
)

// allowPrivateBaseURLEnv is the escape hatch for deployments that legitimately
// point ANTHROPIC_BASE_URL at an internal HTTPS gateway/proxy on an RFC1918
// address (e.g. an in-cluster bedrock-proxy). When set to a truthy value the
// https branch skips the private-IP SSRF guard. The IMDS metadata address
// (169.254.169.254) and link-local ranges are ALWAYS rejected regardless —
// there is no legitimate reason to base a Claude endpoint there, and that is
// the high-value SSRF target. R202606e-SEC-1 (#2278).
func allowPrivateBaseURL() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NAOZHI_ALLOW_PRIVATE_BASE_URL"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

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
			// RFC1918 / unique-local / loopback private ranges are rejected
			// to stop a poisoned parent env pointing the base URL at an
			// internal HTTPS service for SSRF. Operators with a legitimate
			// internal HTTPS gateway opt out via NAOZHI_ALLOW_PRIVATE_BASE_URL.
			// R202606e-SEC-1 (#2278).
			if (ip.IsPrivate() || ip.IsLoopback()) && !allowPrivateBaseURL() {
				return fmt.Errorf("private-range host %q rejected (SSRF guard); set NAOZHI_ALLOW_PRIVATE_BASE_URL=1 to allow", host)
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
