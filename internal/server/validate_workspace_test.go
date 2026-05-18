package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestValidateWorkspace_Sentinels pins the four error classes that
// classifyWorkspaceErr depends on. Without these, a future refactor that
// merges them back into one generic string would silently regress the
// dashboard's "为什么 work_dir 被拒" UX 修复（前端依赖 raw substring 区分
// invalid / not_exist / not_dir / outside_root 四种文案）。
func TestValidateWorkspace_Sentinels(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := os.MkdirAll(good, 0o700); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(root, "afile")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		workspace string
		root      string
		wantErr   error
	}{
		{"empty", "", root, ErrWorkspaceInvalid},
		{"relative", "naozhi", root, ErrWorkspaceInvalid},
		{"dot", ".", root, ErrWorkspaceInvalid},
		{"missing", filepath.Join(root, "no-such-dir"), root, ErrWorkspaceNotExist},
		{"file-not-dir", notDir, root, ErrWorkspaceNotDir},
		{"outside-root", "/tmp", root, ErrWorkspaceOutsideRoot},
		{"good", good, root, nil},
		{"root-itself", root, root, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := validateWorkspace(tc.workspace, tc.root)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidateWorkspace_SymlinkRoot is the regression for the bug where
// allowedRoot itself contained a symlink component (e.g. `/home → /var/home`
// on some distros, Docker bind mounts, custom AMI layouts). Before the fix,
// EvalSymlinks resolved wsPath to `/var/home/...` while allowedRoot stayed
// `/home/...`, so HasPrefix failed and every legitimate work_dir was rejected
// as "outside allowed root". Cron's workDirUnderRoot already EvalSymlinks-ed
// both sides; server.validateWorkspace had drifted.
func TestValidateWorkspace_SymlinkRoot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	// Real directory tree: <tmp>/real/{root,root/proj}
	realRoot := filepath.Join(tmp, "real", "root")
	proj := filepath.Join(realRoot, "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	// Symlink alias: <tmp>/alias-root → <tmp>/real/root
	aliasRoot := filepath.Join(tmp, "alias-root")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	// Caller hands us the workspace as <alias>/proj and the configured root
	// as <alias>. validateWorkspace must accept this — both EvalSymlinks
	// down to the same canonical real path.
	resolved, err := validateWorkspace(filepath.Join(aliasRoot, "proj"), aliasRoot)
	if err != nil {
		t.Fatalf("expected accept for symlinked root, got %v", err)
	}
	// On macOS t.TempDir() returns a path under /var/folders which itself
	// resolves to /private/var/folders. Resolve the expected path the same
	// way validateWorkspace does so the assertion is platform-portable.
	want, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatalf("EvalSymlinks(proj): %v", err)
	}
	if resolved != want {
		t.Errorf("resolved = %q, want %q", resolved, want)
	}
}

// TestValidateWorkspace_SymlinkEscape locks down the case where a symlink
// inside allowedRoot points OUT of the root. Resolving wsPath via
// EvalSymlinks lands the canonical target outside rootResolved, and the
// HasPrefix check must reject it as ErrWorkspaceOutsideRoot — even though
// the literal lexical path looks like it lives under root.
func TestValidateWorkspace_SymlinkEscape(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	// Symlink trap: <root>/escape → <tmp>/outside
	trap := filepath.Join(root, "escape")
	if err := os.Symlink(outside, trap); err != nil {
		t.Fatal(err)
	}
	_, err := validateWorkspace(trap, root)
	if !errors.Is(err, ErrWorkspaceOutsideRoot) {
		t.Fatalf("symlink escape should be rejected, got %v", err)
	}
}

// TestClassifyWorkspaceErr asserts the (status, msg) mapping is stable —
// dashboard.js localizeAPIError and the cron handler both depend on the
// exact substrings here.
func TestClassifyWorkspaceErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err        error
		wantStatus int
		wantMsg    string
	}{
		{ErrWorkspaceOutsideRoot, 403, "work_dir outside allowed root"},
		{ErrWorkspaceNotExist, 400, "work_dir does not exist"},
		{ErrWorkspaceNotDir, 400, "work_dir is not a directory"},
		{ErrWorkspaceInvalid, 400, "work_dir is not a valid path"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.err.Error(), func(t *testing.T) {
			t.Parallel()
			gotStatus, gotMsg := classifyWorkspaceErr(tc.err)
			if gotStatus != tc.wantStatus || gotMsg != tc.wantMsg {
				t.Fatalf("classifyWorkspaceErr(%v) = (%d, %q), want (%d, %q)",
					tc.err, gotStatus, gotMsg, tc.wantStatus, tc.wantMsg)
			}
		})
	}
}
