package node

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// validatePeerURL screens a remote peer URL (every doRequest carries the
// dashboard bearer token, so an unvalidated URL is an authenticated SSRF
// vector, #1548) and returns the cleaned scheme://host[:port]. Policy: must be
// absolute http/https with a host; link-local is HARD-REJECTED (IMDS
// 169.254.169.254 / fe80::/10 live there, no peer ever does); loopback and
// RFC1918 are ALLOWED (documented multi-node topology). A DNS hostname passes
// here and is re-screened by safeDialContext on the RESOLVED IP (#1677).
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
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if isBlockedPeerAddr(addr) {
			return "", fmt.Errorf("peer URL host %q is link-local (IMDS/SSRF range), refused", host)
		}
	}
	// Drop any path/query so n.URL+path concatenation stays well-formed.
	base := &url.URL{Scheme: u.Scheme, Host: u.Host}
	return strings.TrimRight(base.String(), "/"), nil
}

// isBlockedPeerAddr is the single source of truth for "never a naozhi peer":
// link-local only (IMDS lives there); loopback and RFC1918 are intentionally
// allowed. Shared by the config-time and dial-time screens so they cannot drift.
func isBlockedPeerAddr(addr netip.Addr) bool {
	return addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast()
}

// isLocalPeerURL reports whether the cleaned peer URL is a loopback or private
// literal IP — the ranges where a config-write attacker would aim the
// bearer-token client at a co-located service, hence doRequest's body cap
// (#1825). A DNS hostname is non-local: the operator's explicit choice.
func isLocalPeerURL(cleanURL string) bool {
	u, err := url.Parse(cleanURL)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate()
}

// safeDialContext refuses a hostname that RESOLVES to a blocked (link-local /
// IMDS) address before any TCP connection opens; validatePeerURL only sees the
// config string (#1677).
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
	// Dial the screened IPs directly so a second lookup cannot return a
	// different (blocked) answer.
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
