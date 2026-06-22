package discovery

import (
	"path/filepath"
	"strings"
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
		{"codex binary", "/home/u/.nvm/versions/node/v22/bin/codex", "codex", true},
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

// TestDetectCLIName_BasenameTruncation pins the contract that the per-OS
// detectCLIName implementations pass the process BASENAME (argv[0] only) to
// DetectInProc — NOT the full command line. proc_linux.go truncates
// /proc/PID/cmdline at the first NUL; proc_darwin.go truncates `ps -o
// command=` at the first space; both then filepath.Base the result. A
// predicate that needs a subcommand token (e.g. codex's old
// `&& Contains("app-server")`) can therefore NEVER match a real process,
// because argv[1] is stripped before it sees the string. This test mirrors
// that truncation so a regression in any predicate is caught (the original
// test fed full paths and so missed the codex bug entirely).
func TestDetectCLIName_BasenameTruncation(t *testing.T) {
	if len(backend.All()) == 0 {
		backend.RegisterDefaults()
	}

	// classify replicates detectCLIName's argv[0]→basename reduction so the
	// predicate sees exactly what production passes it.
	classify := func(rawCmdline string) string {
		s := rawCmdline
		if i := strings.IndexAny(s, "\x00 "); i >= 0 { // NUL (linux) or space (darwin)
			s = s[:i]
		}
		bin := filepath.Base(s)
		for _, p := range backend.All() {
			if p.DetectInProc != nil && p.DetectInProc(bin) {
				return p.DisplayName
			}
		}
		return "cli"
	}

	cases := []struct {
		name       string
		rawCmdline string // full command line as the OS reports it (argv joined)
		want       string
	}{
		// The exact shape naozhi spawns: `codex app-server -c ...`. argv[1]
		// (app-server) must NOT be required — basename is just "codex".
		{"codex app-server (linux NUL-joined)", "/home/u/.nvm/bin/codex\x00app-server\x00-c\x00model=openai.gpt-5.5", "codex"},
		{"codex app-server (darwin space-joined)", "/home/u/.nvm/bin/codex app-server -c model=x", "codex"},
		{"bare codex", "/usr/local/bin/codex", "codex"},
		{"kiro-cli acp (space-joined)", "/home/u/.local/bin/kiro-cli acp --model x", "kiro"},
		{"claude stream-json (NUL-joined)", "/usr/local/bin/claude\x00-p\x00--output-format\x00stream-json", "claude-code"},
		{"unrelated", "/usr/bin/python3 server.py", "cli"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.rawCmdline); got != c.want {
				t.Errorf("classify(%q) = %q; want %q", c.rawCmdline, got, c.want)
			}
		})
	}
}
