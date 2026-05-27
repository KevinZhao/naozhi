package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveTrustedBin_PrefersAbsolutePath pins R237-SEC-6 / #652: when a
// candidate binary exists at /usr/bin (or another well-known absolute
// location), resolveTrustedBin must return the absolute path rather than
// falling through to exec.LookPath. This closes the PATH-poisoning vector
// where an attacker who can prepend a writable directory to PATH would
// otherwise hijack the systemctl/launchctl invocation issued by self-
// update.
//
// We pick "sh" as a probe: it is guaranteed to exist at /bin/sh on every
// supported platform (Linux, Darwin) and is independent of selfupdate's
// real callers. The test would still pass if /bin/sh did not exist (the
// fallback path returns whatever LookPath finds), so we skip in that
// rare case rather than asserting an absolute path.
func TestResolveTrustedBin_PrefersAbsolutePath(t *testing.T) {
	candidates := []string{"/usr/bin/sh", "/bin/sh", "/usr/sbin/sh", "/sbin/sh"}
	var existing string
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			existing = p
			break
		}
	}
	if existing == "" {
		t.Skip("no /usr/bin/sh or /bin/sh on this host; nothing to assert")
	}

	// Poison PATH with a temp dir that contains a fake "sh" executable.
	// If resolveTrustedBin used exec.LookPath instead of stat-first, it
	// would return the poisoned path.
	poison := t.TempDir()
	fake := filepath.Join(poison, "naozhi-test-bin-xyzzy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	// Use a unique name so we don't collide with the cache from any
	// previous test run; the cache key is the bin name. Place a copy of
	// the poison "shadow" at <poison>/sh so a PATH-only resolver would
	// pick it up.
	shPoison := filepath.Join(poison, "sh")
	if err := os.WriteFile(shPoison, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write poison sh: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", poison+":"+origPath)
	defer os.Setenv("PATH", origPath)

	// Reset the global cache so this test sees a fresh resolution.
	// resolveTrustedBin caches per-name via sync.Once, and other tests
	// in this package may have populated it already.
	binCacheMu.Lock()
	delete(binCache, "sh")
	binCacheMu.Unlock()

	got := resolveTrustedBin("sh")
	// Must be an absolute path under /usr/bin, /bin, /usr/sbin, or /sbin —
	// NOT the poisoned PATH dir.
	if !strings.HasPrefix(got, "/usr/bin/") &&
		!strings.HasPrefix(got, "/bin/") &&
		!strings.HasPrefix(got, "/usr/sbin/") &&
		!strings.HasPrefix(got, "/sbin/") {
		t.Errorf("resolveTrustedBin(%q) = %q, want a canonical /usr/bin or /bin path (not PATH-resolved %q)", "sh", got, shPoison)
	}
	if got == shPoison {
		t.Errorf("resolveTrustedBin returned poisoned PATH binary %q — PATH-poisoning vector still open", got)
	}
}

// TestResolveTrustedBin_FallsBackToLookPath covers the operator-with-
// non-standard-install-layout case: when none of the canonical absolute
// paths contain the binary, resolveTrustedBin falls back to exec.LookPath
// so upgrade still works on /opt/-style installs.
func TestResolveTrustedBin_FallsBackToLookPath(t *testing.T) {
	// Use a name that almost certainly does NOT exist under /usr/bin
	// or /bin on a stock host but DOES exist in our temp dir.
	uniqueName := "naozhi_lookpath_fallback_probe_zzzzz"
	tmp := t.TempDir()
	target := filepath.Join(tmp, uniqueName)
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write probe bin: %v", err)
	}
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmp+":"+origPath)
	defer os.Setenv("PATH", origPath)

	// Ensure cache is fresh.
	binCacheMu.Lock()
	delete(binCache, uniqueName)
	binCacheMu.Unlock()

	got := resolveTrustedBin(uniqueName)
	if got != target {
		t.Errorf("resolveTrustedBin(%q) = %q, want %q via LookPath fallback", uniqueName, got, target)
	}
}
