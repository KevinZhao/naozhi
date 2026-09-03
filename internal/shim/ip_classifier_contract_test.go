package shim

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHandRolledIPClassifier pins the #2300 contract: internal/shim must
// not carry its own IP-range classifier. Every "is this IP internal /
// private / link-local / loopback?" decision goes through
// envpolicy.ClassifyIP / ClassifyHost so the shim endpoint SSRF guard cannot
// drift from the envpolicy and node guards on range membership.
//
// The scan is over production sources only (tests may use whatever they
// like). It forbids the stdlib range predicates, CIDR parsing, and the
// private/link-local CIDR literals a hand-rolled table would contain.
func TestNoHandRolledIPClassifier(t *testing.T) {
	t.Parallel()
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
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Comments may mention the ranges; only code lines are checked.
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
