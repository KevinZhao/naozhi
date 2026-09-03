package envpolicy

import "net"

// IPClass is a bit-set of the SSRF-relevant address ranges a literal IP falls
// in. It is the single classification leaf shared by every naozhi
// "is this IP internal?" decision (base-URL env guard here, shim endpoint
// guard in internal/shim, /ws-node Host classification in internal/node) so
// the three sites cannot drift apart on what "private" or "link-local" means
// (#2300, RFC envpolicy-consolidation Phase 2).
//
// The leaf only CLASSIFIES; each caller keeps its own policy (which classes
// to reject, which to allow, whether a non-literal hostname fails open or
// closed). That is deliberate: the sites have different threat models (an
// outbound API target vs. an inbound Host header) and intentionally different
// deny-sets, and this package must stay free of per-caller policy switches.
//
// Semantics are exactly those of the net.IP predicates (IsLoopback,
// IsPrivate, IsLinkLocalUnicast, IsLinkLocalMulticast, IsUnspecified), so an
// IPv4-mapped IPv6 literal such as ::ffff:10.0.0.1 classifies the same as
// 10.0.0.1 — net.ParseIP always yields the 16-byte form and every predicate
// unmaps via To4 first. Callers that hold a netip.Addr must convert (and
// Unmap) before calling; netip's IsPrivate does NOT unmap 4-in-6.
type IPClass uint8

const (
	// IPLoopback is 127.0.0.0/8 or ::1.
	IPLoopback IPClass = 1 << iota
	// IPLinkLocalUnicast is 169.254.0.0/16 (cloud IMDS 169.254.169.254 lives
	// here) or fe80::/10.
	IPLinkLocalUnicast
	// IPLinkLocalMulticast is 224.0.0.0/24 or ff02::/16.
	IPLinkLocalMulticast
	// IPPrivate is RFC1918 (10/8, 172.16/12, 192.168/16) or IPv6 unique-local
	// fc00::/7.
	IPPrivate
	// IPUnspecified is 0.0.0.0 or ::. Binding-address literals that some
	// stacks route to "this host"; never a legitimate remote endpoint.
	IPUnspecified
)

// IPLinkLocal is any link-local address, unicast or multicast. Both are
// always-rejected by every outbound-target guard because the IMDS SSRF pivot
// lives in this range.
const IPLinkLocal = IPLinkLocalUnicast | IPLinkLocalMulticast

// IPInternalAny is the widest deny-set: every non-public range this package
// knows about. This is the shim endpoint guard's deny-set (loopback is
// subtracted at the call site for local mock gateways) and matches the
// weixin/discord/config/selfupdate outbound guards' hand-written predicate.
const IPInternalAny = IPLoopback | IPLinkLocal | IPPrivate | IPUnspecified

// Has reports whether every bit in c is set in k.
func (k IPClass) Has(c IPClass) bool { return k&c == c }

// Any reports whether at least one bit in c is set in k.
func (k IPClass) Any(c IPClass) bool { return k&c != 0 }

// ClassifyIP returns the IPClass bits for ip. A nil or malformed ip yields 0
// (no class) — callers must decide themselves whether "not a literal IP" fails
// open (treat as a hostname resolved elsewhere) or closed; this leaf does not
// choose for them.
func ClassifyIP(ip net.IP) IPClass {
	if ip == nil {
		return 0
	}
	var k IPClass
	if ip.IsLoopback() {
		k |= IPLoopback
	}
	if ip.IsLinkLocalUnicast() {
		k |= IPLinkLocalUnicast
	}
	if ip.IsLinkLocalMulticast() {
		k |= IPLinkLocalMulticast
	}
	if ip.IsPrivate() {
		k |= IPPrivate
	}
	if ip.IsUnspecified() {
		k |= IPUnspecified
	}
	return k
}

// ClassifyHost parses host as an IP literal (no port, no brackets — pass the
// result of url.URL.Hostname or an already-stripped Host header) and returns
// its class. ok is false when host is not an IP literal, in which case the
// returned class is 0 and the caller is looking at a DNS name whose
// resolution is not known at this layer.
func ClassifyHost(host string) (k IPClass, ok bool) {
	ip := net.ParseIP(host)
	if ip == nil {
		return 0, false
	}
	return ClassifyIP(ip), true
}
