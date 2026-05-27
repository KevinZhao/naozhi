package upstream

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

// TestSanitizeWorkspacePath_AcceptsExistingPathInsideRoot exercises the
// shared helper that #709 (R237-CR-6) extracted from the three duplicated
// EvalSymlinks + Clean + IsAbs + allowed-root call sites in
// send/takeover/close_discovered. The helper must accept canonical
// in-root paths verbatim.
func TestSanitizeWorkspacePath_AcceptsExistingPathInsideRoot(t *testing.T) {
	// Resolve t.TempDir() through the same EvalSymlinks the helper applies
	// to its input — on macOS /var/folders/... is a symlink to
	// /private/var/folders/..., so feeding the raw t.TempDir() to
	// defaultWorkspace would make the prefix gate compare resolved-vs-raw
	// and reject every in-root path. Production wires defaultWorkspace
	// from a path that has already cleared this resolution.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	sub := filepath.Join(root, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := &Connector{cfg: &config.UpstreamConfig{}, defaultWorkspace: root}
	got, err := c.sanitizeWorkspacePath(sub, "workspace", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != sub {
		t.Errorf("got %q, want %q", got, sub)
	}
}

// Outside-root paths must be rejected with an "outside allowed root"
// message so operators can grep for it.
func TestSanitizeWorkspacePath_RejectsOutsideAllowedRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	// A sibling tempdir, definitely outside root. Resolved likewise so
	// the helper's EvalSymlinks does not coincidentally yield root's
	// canonical form.
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(other): %v", err)
	}
	c := &Connector{cfg: &config.UpstreamConfig{}, defaultWorkspace: root}
	_, err = c.sanitizeWorkspacePath(other, "workspace", false)
	if err == nil {
		t.Fatal("expected outside-root error, got nil")
	}
	if !strings.Contains(err.Error(), "outside allowed root") {
		t.Errorf("err = %v, want 'outside allowed root'", err)
	}
}

// tolerateMissing=false (send / takeover policy) must surface ENOENT as
// an error; the path doesn't exist so we cannot trust it.
func TestSanitizeWorkspacePath_StrictMissing(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	missing := filepath.Join(root, "does-not-exist")
	c := &Connector{cfg: &config.UpstreamConfig{}, defaultWorkspace: root}
	_, err = c.sanitizeWorkspacePath(missing, "workspace", false)
	if err == nil {
		t.Fatal("expected error for missing path under strict mode, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want wrapped fs.ErrNotExist", err)
	}
}

// tolerateMissing=true (close_discovered policy) must accept a vanished
// directory by falling back to the cleaned syntactic path while still
// enforcing the allowed-root prefix gate.
func TestSanitizeWorkspacePath_TolerateMissingFallsBackButStillEnforcesRoot(t *testing.T) {
	root := t.TempDir()
	wantRoot, _ := filepath.EvalSymlinks(root)
	missingInside := filepath.Join(wantRoot, "vanished")
	c := &Connector{cfg: &config.UpstreamConfig{}, defaultWorkspace: wantRoot}

	got, err := c.sanitizeWorkspacePath(missingInside, "close_discovered cwd", true)
	if err != nil {
		t.Fatalf("expected ENOENT to be tolerated, got %v", err)
	}
	if got != missingInside {
		t.Errorf("got %q, want %q", got, missingInside)
	}

	// Even with tolerateMissing=true, an outside-root vanished path must
	// still be rejected — the prefix gate is the load-bearing defense
	// that prevents primary-controlled "/etc/passwd" payloads slipping
	// through after a directory is gone.
	outsideMissing := "/definitely-not-under-root/vanished"
	if _, err := c.sanitizeWorkspacePath(outsideMissing, "close_discovered cwd", true); err == nil {
		t.Fatal("expected outside-root rejection even with tolerateMissing=true, got nil")
	}
}

// Error messages must carry the kind label so each call site retains its
// distinct operator-facing identity (workspace vs. takeover cwd vs.
// close_discovered cwd).
func TestSanitizeWorkspacePath_ErrorIncludesKindLabel(t *testing.T) {
	root := t.TempDir()
	c := &Connector{cfg: &config.UpstreamConfig{}, defaultWorkspace: root}
	for _, kind := range []string{"workspace", "takeover cwd", "close_discovered cwd"} {
		_, err := c.sanitizeWorkspacePath("/etc", kind, false)
		if err == nil {
			t.Fatalf("kind=%q: expected error", kind)
		}
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("kind=%q: err = %v, want to contain kind label", kind, err)
		}
	}
}
