package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStrayConfigGetenv enforces the ARCH5 / #385 config-precedence
// contract for the internal/server package: configuration must flow through
// the documented sources (config.yaml + ~/.claude/settings.json env), and the
// only sanctioned direct os.Getenv in this package is the single
// CLAUDE_PROJECTS_DIR resolver in claude_paths.go (the documented transcripts
// override, R222-ARCH-9 / #724). Any new os.Getenv config read added here is a
// regression of the bypass tracked by #385 — see
// docs/design/config-precedence.md. This guard scans the package's own
// non-test .go source (NOT vendored / generated code) so the bypass cannot
// creep back in silently.
func TestNoStrayConfigGetenv(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	const sanctioned = `os.Getenv("CLAUDE_PROJECTS_DIR")`
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(data)
		// Walk every os.Getenv occurrence and require it to be the one
		// sanctioned resolver. We intentionally do a literal scan rather
		// than AST parsing: the contract is "no NEW env reads", and a
		// literal match keeps the guard cheap and obvious.
		for idx := 0; ; {
			i := strings.Index(src[idx:], "os.Getenv(")
			if i < 0 {
				break
			}
			at := idx + i
			// Reconstruct the call as `os.Getenv("...")` up to the close paren.
			end := strings.IndexByte(src[at:], ')')
			if end < 0 {
				t.Fatalf("%s: malformed os.Getenv call near offset %d", name, at)
			}
			call := src[at : at+end+1]
			if call != sanctioned {
				t.Errorf("%s: stray config os.Getenv %q bypasses the config layer (ARCH5/#385). "+
					"Route naozhi config through config.yaml / ~/.claude settings, or add it to "+
					"docs/design/config-precedence.md with a single resolver and update this allowlist.",
					name, call)
			}
			idx = at + len(call)
		}
	}
}

// TestResolveClaudeDir_HomeFallback confirms the helper joins ~/.claude
// when UserHomeDir succeeds. Pins R222-ARCH-9 / #724 single-source probe
// against future drift.
func TestResolveClaudeDir_HomeFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp) // os.UserHomeDir on linux honours $HOME
	got := resolveClaudeDir()
	want := filepath.Join(tmp, ".claude")
	if got != want {
		t.Errorf("resolveClaudeDir() = %q, want %q", got, want)
	}
}

// TestResolveClaudeProjectsDir_EnvOverride confirms CLAUDE_PROJECTS_DIR
// short-circuits the home probe so deployments that pin transcripts under
// /var/lib/claude do not need to symlink ~/.claude/projects. The env
// override existed before R222-ARCH-9 / #724; this test pins the
// short-circuit so the consolidation does not silently drop it.
func TestResolveClaudeProjectsDir_EnvOverride(t *testing.T) {
	override := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", override)
	// Even with HOME pointing elsewhere, the env override wins.
	t.Setenv("HOME", t.TempDir())
	got := resolveClaudeProjectsDir()
	if got != override {
		t.Errorf("resolveClaudeProjectsDir() with override = %q, want %q",
			got, override)
	}
}

// TestResolveClaudeProjectsDir_HomeFallback confirms ~/.claude/projects is
// the default when the env override is absent.
func TestResolveClaudeProjectsDir_HomeFallback(t *testing.T) {
	if err := os.Unsetenv("CLAUDE_PROJECTS_DIR"); err != nil {
		t.Fatalf("unset CLAUDE_PROJECTS_DIR: %v", err)
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	got := resolveClaudeProjectsDir()
	want := filepath.Join(tmp, ".claude", "projects")
	if got != want {
		t.Errorf("resolveClaudeProjectsDir() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join(".claude", "projects")) {
		t.Errorf("resolveClaudeProjectsDir() = %q missing .claude/projects suffix", got)
	}
}
