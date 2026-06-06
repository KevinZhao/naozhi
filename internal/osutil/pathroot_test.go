package osutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPathContainedInRoot_ByteLevel covers the platform-independent prefix
// semantics that hold on every filesystem (case-sensitive or not). These
// assertions migrate the original TestSameFileAncestor coverage from
// internal/dashboard/cron after the helper was hoisted here (the
// SHARED-ALGORITHM-WITH-SERVER contract — all callers now share this impl).
func TestPathContainedInRoot_ByteLevel(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "claude", "projects")
	deep := filepath.Join(root, "slug", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	if err := os.WriteFile(deep, []byte("x"), 0o600); err != nil {
		t.Fatalf("write deep: %v", err)
	}
	outside := filepath.Join(tmp, "elsewhere", "x.jsonl")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	if !PathContainedInRoot(root, root) {
		t.Errorf("root == resolved must be contained")
	}
	if !PathContainedInRoot(deep, root) {
		t.Errorf("deep child must be contained under root")
	}
	if PathContainedInRoot(outside, root) {
		t.Errorf("path outside root must not be contained")
	}
	if PathContainedInRoot(deep, filepath.Join(tmp, "does-not-exist")) {
		t.Errorf("missing root must return false rather than panic")
	}
}

// TestPathContainedInRoot_SiblingPrefixTrap pins the classic
// "/var/foo" prefix-matches "/var/fooBar" bug: the separator anchor in the
// HasPrefix branch must reject a sibling whose name merely starts with root's
// basename. Both paths are constructed (need not exist) — this exercises the
// pure string branch, which never Stats when HasPrefix already says "no".
func TestPathContainedInRoot_SiblingPrefixTrap(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "var", "foo")
	sibling := filepath.Join(string(filepath.Separator), "var", "fooBar")
	if PathContainedInRoot(sibling, root) {
		t.Errorf("sibling %q must not be contained in root %q", sibling, root)
	}
}

// TestPathContainedInRoot_InodeFallback is the platform-independent proxy for
// the macOS/Windows case-fold bug. The real bug is "resolved and root differ
// byte-wise but name the same inode"; on a case-sensitive Linux CI we can
// reproduce that exact condition with a symlink instead of a case variant.
//
// Layout: <tmp>/real/proj is the true directory. <tmp>/alias -> <tmp>/real is
// a symlink. We hand the helper resolved=<tmp>/real/proj (already canonical)
// and root=<tmp>/alias/proj (NOT EvalSymlinks-resolved — byte-different but the
// same inode as <tmp>/real/proj). HasPrefix fails (alias != real), so the
// inode walk must rescue it: os.Stat(root) follows the symlink to the same
// inode as resolved, and the walk finds a SameFile match at the first level.
func TestPathContainedInRoot_InodeFallback(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	realProj := filepath.Join(tmp, "real", "proj")
	if err := os.MkdirAll(realProj, 0o755); err != nil {
		t.Fatalf("mkdir realProj: %v", err)
	}
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(filepath.Join(tmp, "real"), alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(realProj)
	if err != nil {
		t.Fatalf("EvalSymlinks(realProj): %v", err)
	}
	aliasProj := filepath.Join(alias, "proj") // byte-different, same inode
	if !PathContainedInRoot(resolved, aliasProj) {
		t.Errorf("inode-equal root %q must contain resolved %q via SameFile walk", aliasProj, resolved)
	}
}

// TestPathContainedInRoot_SymlinkEscapeStaysRejected guards the security
// contract: a resolved path whose real location is outside root must be
// rejected even though the inode walk is now in play. Because callers
// EvalSymlinks first, an escaping symlink resolves to its true out-of-root
// target; neither the byte prefix nor the inode walk can map it back under
// root.
func TestPathContainedInRoot_SymlinkEscapeStaysRejected(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	outside := filepath.Join(tmp, "outside", "secret")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	// resolved is the real out-of-root path (what EvalSymlinks would yield
	// for a <root>/escape -> <tmp>/outside/secret symlink).
	resolvedOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("EvalSymlinks(outside): %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	if PathContainedInRoot(resolvedOutside, resolvedRoot) {
		t.Errorf("out-of-root path %q must not be contained in %q", resolvedOutside, resolvedRoot)
	}
}

// TestSameFileAncestor_SymlinkMidPath pins R112714-SEC-2: sameFileAncestor
// must use os.Lstat (not os.Stat) when walking the ancestor chain so a
// symlink planted at an intermediate path component cannot redirect the
// inode comparison to an attacker-controlled directory.
//
// Scenario: root = /tmp/.../allowed
//
//	target = /tmp/.../allowed/real/file
//	mid symlink: /tmp/.../allowed/link -> /tmp/.../outside
//
// With os.Stat the loop would stat the symlink target (/tmp/.../outside)
// and compare it against rootInfo — a false-ancestor match if the attacker
// owns 'outside'. With os.Lstat the loop stats the symlink node itself,
// which differs from rootInfo, so it climbs to the next parent (allowed/)
// and correctly returns true (or false when the symlink points outside).
//
// Migrated from internal/dashboard/cron when sameFileAncestor was hoisted
// into osutil (SHARED-ALGORITHM-WITH-SERVER dedup); coverage and the Lstat
// hardening now live with the single shared implementation.
func TestSameFileAncestor_SymlinkMidPath(t *testing.T) {
	if os.Getenv("CI_SKIP_SYMLINK") != "" {
		t.Skip("symlink tests disabled")
	}
	tmp := t.TempDir()
	allowed := filepath.Join(tmp, "allowed")
	outside := filepath.Join(tmp, "outside")

	if err := os.MkdirAll(filepath.Join(allowed, "real"), 0o755); err != nil {
		t.Fatalf("mkdir allowed/real: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}

	realFile := filepath.Join(allowed, "real", "file.jsonl")
	if err := os.WriteFile(realFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}

	// Symlink: allowed/link -> ../outside (points outside the allowed root).
	linkPath := filepath.Join(allowed, "link")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink creation not supported: %v", err)
	}

	// A file path through the symlink component (allowed/link/x.jsonl).
	// sameFileAncestor walks the LEXICAL parents of the input, not symlink
	// targets: it Lstats allowed/link/x.jsonl (no-exist → err), climbs to
	// allowed/link (a symlink inode ≠ rootInfo), then to allowed/ (inode ==
	// rootInfo → true). With Stat, 'allowed/link' would resolve to 'outside'
	// and could match an attacker-planted inode; Lstat does not follow it.
	symlinkChildPath := filepath.Join(linkPath, "x.jsonl")
	if !sameFileAncestor(symlinkChildPath, allowed) {
		t.Errorf("sameFileAncestor(%q, %q) = false; expected true because the "+
			"lexical path is under 'allowed/' even though an intermediate "+
			"component is a symlink pointing outside", symlinkChildPath, allowed)
	}

	// A path genuinely outside the root is still rejected.
	outsideFile := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(outsideFile, []byte("y"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if sameFileAncestor(outsideFile, allowed) {
		t.Errorf("sameFileAncestor(%q, %q) = true; path outside root must be rejected",
			outsideFile, allowed)
	}
}
