package discovery

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/backend"
)

// TestDetectCLIName_ScansProfileRegistry pins the Sprint 1b refactor: the
// per-OS detectCLIName implementations no longer hardcode "kiro" / "claude"
// — they iterate backend.All() and return the first matching profile's
// DisplayName. This test exercises the iteration logic by registering
// dummy profiles via the registry's reset+register cycle.
//
// Cannot test the OS-specific detectCLIName function directly without a
// running PID; the contract here is that backend.All() returns the
// expected profiles and DetectInProc predicates classify cmdlines correctly.
// See docs/rfc/multi-backend.md §3.4.
func TestDetectCLIName_ScansProfileRegistry(t *testing.T) {
	// RegisterDefaults is idempotent here because a previous test in the
	// suite may have already registered, and Register panics on dup. We
	// rely on the package-level registry being seeded once for the whole
	// test run (matching production startup).
	if len(backend.All()) == 0 {
		backend.RegisterDefaults()
	}

	cases := []struct {
		name    string
		cmdline string
		wantBin string // expected DisplayName
		wantHit bool
	}{
		{"claude binary", "/usr/local/bin/claude", "claude-code", true},
		{"kiro-cli binary", "/home/u/.local/bin/kiro-cli", "kiro", true},
		{"kiro-cli-chat", "kiro-cli-chat", "kiro", true},
		{"kiro-cli-term", "kiro-cli-term", "kiro", true},
		{"unrelated binary", "/usr/bin/bash", "", false},
		{"empty", "", "", false},
		// claude profile's DetectInProc is "contains claude && !contains kiro"
		// so a hypothetical "claude-kiro-fake" must NOT match claude.
		{"claude-kiro hybrid filename", "claude-kiro-fake", "kiro", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			for _, p := range backend.All() {
				if p.DetectInProc != nil && p.DetectInProc(c.cmdline) {
					got = p.DisplayName
					break
				}
			}
			if c.wantHit {
				if got != c.wantBin {
					t.Errorf("backend.All loop on %q matched %q; want %q", c.cmdline, got, c.wantBin)
				}
			} else if got != "" {
				t.Errorf("backend.All loop on %q matched %q; want no match", c.cmdline, got)
			}
		})
	}
}
