package node

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// errPeerURLInvalid carries why a node peer URL was rejected at construction
// time. doRequest returns it (wrapped) so an operator who pasted a bad/unsafe
// URL into config sees a clean failure instead of a full-authenticated SSRF
// request leaving the host. nil means the URL passed validation.
//
// R20260601-SEC-2 (#1548): every doRequest attaches the dashboard Bearer
// token, so an unvalidated peer URL was a full-authenticated SSRF vector —
// a tampered/misconfigured config could point n.URL at
// http://169.254.169.254/latest/meta-data/ (cloud IMDS) and exfiltrate
// credentials. CheckRedirect already blocks the redirect-pivot variant; this
// guards the *initial* request target.

// validatePeerURL parses and screens a remote naozhi peer URL. It returns the
// cleaned base URL (scheme://host[:port], no trailing slash) on success.
//
// Policy (see #1548 design decision):
//   - MUST be an absolute http/https URL with a host. Anything else (file://,
//     gopher://, empty host, relative) is rejected — these are never a valid
//     naozhi peer and are the classic SSRF scheme-pivot payloads.
//   - Link-local addresses are HARD-REJECTED. The IMDS endpoints
//     (IPv4 169.254.169.254 inside 169.254.0.0/16, IPv6 fe80::/10) live here
//     and a naozhi peer is never reachable over a link-local address, so
//     there is no legitimate-deployment cost to blocking the whole range.
//   - Loopback (127.0.0.0/8, ::1) and RFC1918 private ranges are ALLOWED:
//     local multi-node loopback bridging and private-LAN peers are the
//     documented, supported topology. Blocking them would break real
//     deployments, so they pass (the Bearer token + TLS-min-version already
//     bound the residual risk for those links).
//
// A literal IP host is screened directly. A DNS hostname is allowed through
// (resolution happens per-request); rebinding to a link-local address at dial
// time is out of scope here and is mitigated by CheckRedirect + the fixed
// peer-token trust model.
func validatePeerURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("peer URL is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse peer URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("peer URL scheme %q not allowed (want http/https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("peer URL %q has no host", raw)
	}
	// Literal IP host: screen the link-local range directly. A hostname that
	// is not an IP literal falls through (DNS resolution is per-request).
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
			return "", fmt.Errorf("peer URL host %q is link-local (IMDS/SSRF range), refused", host)
		}
	}
	// Rebuild a clean base: scheme://host[:port], dropping any path/query so
	// doRequest's n.URL+path concatenation stays well-formed even if config
	// carried a trailing slash or stray path.
	base := &url.URL{Scheme: u.Scheme, Host: u.Host}
	return strings.TrimRight(base.String(), "/"), nil
}
