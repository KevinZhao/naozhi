package node

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHandRolledIPClassifier pins the #2300 contract for internal/node: the
// /ws-node Host classifiers (isPrivateHost / isLoopbackHost in
// reverseserver.go) must not carry their own IP-range predicates; range
// membership comes from envpolicy.ClassifyHost so it cannot drift from the
// shim/envpolicy SSRF guards.
//
// Exemption: httpclient_validate.go implements the OUTBOUND peer-URL policy
// (isBlockedPeerAddr / isLocalPeerURL) on netip.Addr. It is a distinct,
// documented policy (loopback + RFC1918 peers ALLOWED, link-local hard-
// blocked) and netip's predicates differ from net.IP's on 4-in-6 mapping, so
// migrating it needs its own equivalence review; it is tracked as a #2300
// follow-up rather than swept in here. Do NOT add further files to this list
// without a policy review.
func TestNoHandRolledIPClassifier(t *testing.T) {
	t.Parallel()
	exempt := map[string]bool{
		"httpclient_validate.go": true,
	}
	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`\.IsPrivate\(\)`),
		regexp.MustCompile(`\.IsLoopback\(\)`),
		regexp.MustCompile(`\.IsLinkLocalUnicast\(\)`),
		regexp.MustCompile(`\.IsLinkLocalMulticast\(\)`),
		regexp.MustCompile(`\.IsUnspecified\(\)`),
		regexp.MustCompile(`\bnet\.ParseCIDR\(`),
		regexp.MustCompile(`\bnetip\.MustParsePrefix\(`),
		regexp.MustCompile(`\bnetip\.ParsePrefix\(`),
		regexp.MustCompile(`"(10\.0\.0\.0/8|172\.16\.0\.0/12|192\.168\.0\.0/16|169\.254\.0\.0/16|127\.0\.0\.0/8|fc00::/7|fd00::/8|fe80::/10)"`),
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || exempt[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, re := range forbidden {
				if re.MatchString(line) {
					t.Errorf("%s:%d hand-rolled IP classification %q — use envpolicy.ClassifyHost / ClassifyIP (#2300)", name, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

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
