package node

import "testing"

// TestIsPrivateHost_NarrowerThanOutboundDenySet pins the deliberate policy
// asymmetry documented on isPrivateHost: on the INBOUND /ws-node Host header,
// "private" means "accept plaintext with a warning", so ranges the outbound
// SSRF guards reject (link-local multicast, unspecified, loopback) must NOT be
// classified private here — they fall through to the public hard-reject (or,
// for loopback, to isLoopbackHost). A future "let's align with the shim
// deny-set" edit would silently loosen the gate; this test catches it. #2300.
func TestIsPrivateHost_NarrowerThanOutboundDenySet(t *testing.T) {
	t.Parallel()
	for _, h := range []string{
		"224.0.0.1:8080", // link-local multicast
		"[ff02::1]:8080",
		"0.0.0.0:8080", // unspecified
		"[::]:8080",
		"127.0.0.1:8080", // loopback: isLoopbackHost's job
		"[::1]:8080",
		"[::ffff:0.0.0.0]:8080",
		"[::ffff:127.0.0.1]:8080",
	} {
		if isPrivateHost(h) {
			t.Errorf("isPrivateHost(%q) = true; must stay false (would accept plaintext upgrade)", h)
		}
	}
	// IPv4-mapped IPv6 private/link-local literals classify the same as the
	// bare IPv4 form (net.IP unmaps), preserving pre-#2300 behaviour.
	for _, h := range []string{"[::ffff:10.0.0.1]:8080", "[::ffff:192.168.1.1]", "[::ffff:169.254.1.1]:1"} {
		if !isPrivateHost(h) {
			t.Errorf("isPrivateHost(%q) = false; want true (4-in-6 must unmap)", h)
		}
	}
}
