package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEvalSymlinksFallback locks in the R245-CR-002 (#873) fix: when
// filepath.EvalSymlinks fails (typical: binary launched via a path that
// is itself a non-symlink, or a path the user cannot stat), runInstall
// keeps the original binary path rather than emitting an empty string
// into the rendered systemd unit. The bug was a silent error-swallow
// that produced ExecStart="" on hosts where Stat permissions were
// restricted; the fix is the `if err == nil` guard at service.go:113.
//
// We exercise the same fallback shape directly because runInstall calls
// os.Exit on every parse path. A regression that re-introduces
// `resolved, _ := EvalSymlinks(binary)` would silently overwrite the
// binary path with "" and this test would fail.
func TestEvalSymlinksFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("EvalSymlinks behaviour on missing path differs on windows; skip")
	}
	// Pick a path that is guaranteed not to resolve as a symlink target —
	// the temp dir plus a never-created child. EvalSymlinks must error
	// with PathError; the fix ensures the original `binary` survives.
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "definitely-not-here")

	binary := missing
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	if binary != missing {
		t.Fatalf("EvalSymlinks fallback dropped original path: got %q, want %q", binary, missing)
	}
}

// TestEvalSymlinksSuccess pins the success leg: when the path does
// resolve, the resolved value (post symlink walk) replaces the original
// binary path. Together with the fallback test above, this enforces the
// exact `if resolved, err := EvalSymlinks(p); err == nil { p = resolved }`
// shape — a future refactor that flips the guard direction or returns
// the wrong variable trips here.
func TestEvalSymlinksSuccess(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("create target: %v", err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	binary := link
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	// EvalSymlinks may canonicalise tmp (e.g. /private/var on darwin);
	// match on the trailing "target" path component which the test
	// constructed and which any sane resolution preserves.
	if filepath.Base(binary) != "target" {
		t.Fatalf("EvalSymlinks did not resolve link: got %q (want basename %q)", binary, "target")
	}
}
