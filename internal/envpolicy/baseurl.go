// Package envpolicy holds the shared, leaf-level env-filtering primitives for
// the Claude subprocess env policy: base-URL SSRF/redirect validation, AWS
// profile-name validation, the per-backend raw-credential matrix, and the
// SSRF IP-range classifier. Functions are pure (callers log at the rejection
// site). MUST stay a leaf (no internal/* imports, see imports_test.go)
// because internal/shim and internal/node import it.
package envpolicy

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// allowPrivateBaseURL lets deployments point ANTHROPIC_BASE_URL at an internal
// HTTPS gateway on an RFC1918 address (e.g. an in-cluster bedrock-proxy). IMDS
// and link-local stay ALWAYS rejected — the high-value SSRF target (#2278).
func allowPrivateBaseURL() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NAOZHI_ALLOW_PRIVATE_BASE_URL"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ValidateBaseURLValue enforces https:// for an API base-URL passed to a
// Claude subprocess unless it targets loopback (localhost / 127.0.0.0/8 /
// ::1), where http is allowed for local mock gateways. "" is accepted. (#1687)
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
		if k, ok := ClassifyHost(host); ok {
			if k.Any(IPLinkLocal) {
				return fmt.Errorf("link-local host %q rejected (SSRF/IMDS guard)", host)
			}
			// A poisoned parent env must not aim the base URL at an internal
			// HTTPS service (#2278). IPUnspecified is deliberately NOT rejected
			// here (the shim guard does); policy decision tracked from #2300.
			if k.Any(IPPrivate|IPLoopback) && !allowPrivateBaseURL() {
				return fmt.Errorf("private-range host %q rejected (SSRF guard); set NAOZHI_ALLOW_PRIVATE_BASE_URL=1 to allow", host)
			}
		}
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if k, ok := ClassifyHost(host); ok {
			if k.Has(IPLoopback) {
				return nil
			}
			if k.Any(IPLinkLocal) {
				return fmt.Errorf("link-local host %q rejected (SSRF/IMDS guard)", host)
			}
		}
		return fmt.Errorf("plain http:// to non-loopback host %q rejected (SSRF/redirect guard); use https://", host)
	}
	return fmt.Errorf("scheme %q not allowed; use https://", u.Scheme)
}
