package envpolicy

import "net"

// IPClass is a bit-set of the SSRF-relevant address ranges a literal IP falls
// in — the single classification leaf shared by the base-URL guard, the shim
// endpoint guard and internal/node Host classification (#2300). It only
// CLASSIFIES; each caller keeps its own deny-set because the threat models
// differ. Semantics are those of the net.IP predicates, so ::ffff:10.0.0.1
// classifies like 10.0.0.1; netip.Addr callers must Unmap first.
type IPClass uint8

const (
	// IPLoopback is 127.0.0.0/8 or ::1.
	IPLoopback IPClass = 1 << iota
	// IPLinkLocalUnicast is 169.254.0.0/16 (cloud IMDS lives here) or fe80::/10.
	IPLinkLocalUnicast
	// IPLinkLocalMulticast is 224.0.0.0/24 or ff02::/16.
	IPLinkLocalMulticast
	// IPPrivate is RFC1918 (10/8, 172.16/12, 192.168/16) or IPv6 unique-local fc00::/7.
	IPPrivate
	// IPUnspecified is 0.0.0.0 or ::; never a legitimate remote endpoint.
	IPUnspecified
)

// IPLinkLocal is any link-local address; always rejected by outbound-target
// guards because the IMDS SSRF pivot lives here.
const IPLinkLocal = IPLinkLocalUnicast | IPLinkLocalMulticast

// IPInternalAny is the widest deny-set (the shim endpoint guard's; loopback is
// subtracted at the call site for local mock gateways).
const IPInternalAny = IPLoopback | IPLinkLocal | IPPrivate | IPUnspecified

// Has reports whether every bit in c is set in k.
func (k IPClass) Has(c IPClass) bool { return k&c == c }

// Any reports whether at least one bit in c is set in k.
func (k IPClass) Any(c IPClass) bool { return k&c != 0 }

// ClassifyIP returns the IPClass bits for ip; nil/malformed yields 0 and the
// caller decides whether "not a literal IP" fails open or closed.
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

// ClassifyHost parses host as an IP literal (no port, no brackets); ok is
// false for a DNS name whose resolution is unknown at this layer.
func ClassifyHost(host string) (k IPClass, ok bool) {
	ip := net.ParseIP(host)
	if ip == nil {
		return 0, false
	}
	return ClassifyIP(ip), true
}
