package node

import (
	"context"
	"fmt"
	"net"
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
// here because resolution happens per-request — but the dial-time guard
// (safeDialContext / screenDialAddr) re-screens the *resolved* IP before any
// TCP connection opens, so a hostname that resolves to a link-local/IMDS
// address (DNS-rebinding SSRF, R20260603-SEC-2 #1677) is blocked at dial time,
// not merely at config-parse time.
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
	// is not an IP literal falls through (DNS resolution is per-request, and
	// the resolved IP is re-screened by safeDialContext before TCP open).
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if isBlockedPeerAddr(addr) {
			return "", fmt.Errorf("peer URL host %q is link-local (IMDS/SSRF range), refused", host)
		}
	}
	// Rebuild a clean base: scheme://host[:port], dropping any path/query so
	// doRequest's n.URL+path concatenation stays well-formed even if config
	// carried a trailing slash or stray path.
	base := &url.URL{Scheme: u.Scheme, Host: u.Host}
	return strings.TrimRight(base.String(), "/"), nil
}

// isBlockedPeerAddr is the single source of truth for "this resolved IP must
// never be a naozhi peer". It hard-rejects link-local addresses — the IMDS
// endpoints (IPv4 169.254.169.254 in 169.254.0.0/16, IPv6 fe80::/10) live
// here. Loopback and RFC1918 are intentionally NOT blocked: local multi-node
// loopback bridging and private-LAN peers are the documented, supported
// topology (see validatePeerURL policy). Used by both the literal-IP screen
// and the dial-time guard so config-time and dial-time policy can never drift.
func isBlockedPeerAddr(addr netip.Addr) bool {
	return addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast()
}

// safeDialContext wraps the default dialer so that a DNS hostname which
// resolves to a blocked (link-local/IMDS) address is refused before any TCP
// connection opens. validatePeerURL only sees the config string at construction
// time, so a hostname that resolves to 169.254.169.254 (cloud IMDS) — whether
// by attacker-controlled DNS, DNS rebinding, or a poisoned resolver — would
// otherwise carry the dashboard Bearer token straight to the metadata service.
// R20260603-SEC-2 (#1677).
func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial: split host/port %q: %w", address, err)
	}
	dialer := &net.Dialer{}
	resolver := net.DefaultResolver
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("dial: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedPeerAddr(ip.Unmap()) {
			return nil, fmt.Errorf("dial to %q refused: resolves to link-local/IMDS address %s (SSRF)", host, ip)
		}
	}
	// Re-dial against the already-resolved, screened IPs to avoid a TOCTOU
	// gap where a second resolver lookup could return a different (blocked)
	// answer. Try each screened IP in order.
	var lastErr error
	for _, ip := range ips {
		conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("dial: no addresses for %q", host)
	}
	return nil, lastErr
}
