package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyFileBackup_SymlinkNotFollowed pins [R20260603-SEC-9]: a symlink
// pre-placed at the backup path (predictable .naozhi-upgrade.bak) by a hostile
// UID on a shared install dir must NOT be written through. copyFileBackup
// unlinks the path first (severing the symlink) and opens with O_EXCL, so the
// attacker-controlled target file is left untouched.
func TestCopyFileBackup_SymlinkNotFollowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("backup-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Attacker's victim file the symlink points at.
	victim := filepath.Join(dir, "victim")
	const victimContent = "do-not-overwrite"
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-place a symlink at the backup path pointing at the victim.
	backupPath := filepath.Join(dir, "naozhi"+backupSuffix)
	if err := os.Symlink(victim, backupPath); err != nil {
		t.Fatal(err)
	}

	if err := copyFileBackup(src, backupPath); err != nil {
		t.Fatalf("copyFileBackup: %v", err)
	}

	// Victim must be untouched — the symlink was unlinked, not followed.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != victimContent {
		t.Fatalf("victim was overwritten through symlink: got %q, want %q", got, victimContent)
	}

	// Backup path must now be a regular file holding the source content.
	fi, err := os.Lstat(backupPath)
	if err != nil {
		t.Fatalf("lstat backup: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("backup path is still a symlink after copyFileBackup")
	}
	bak, _ := os.ReadFile(backupPath)
	if string(bak) != "backup-content" {
		t.Fatalf("backup content = %q, want %q", bak, "backup-content")
	}
}

// TestReplace_StaleBackupDoesNotBlock pins [R20260603-SEC-9]: a leftover .bak
// from a prior upgrade (a regular file) must not make the O_EXCL backup open
// fail — copyFileBackup removes the stale file first, so Replace succeeds.
func TestReplace_StaleBackupDoesNotBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	installPath := filepath.Join(dir, "naozhi")
	if err := os.WriteFile(installPath, []byte("current binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale backup left by a previous upgrade run.
	staleBak := installPath + backupSuffix
	if err := os.WriteFile(staleBak, []byte("ancient backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "naozhi-new")
	if err := os.WriteFile(newBin, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	backupPath, err := Replace(newBin, installPath)
	if err != nil {
		t.Fatalf("Replace with stale backup present: %v", err)
	}
	if got, _ := os.ReadFile(installPath); string(got) != "new binary" {
		t.Fatalf("installPath = %q, want %q", got, "new binary")
	}
	// Backup now reflects the binary that was live at upgrade time.
	if bak, _ := os.ReadFile(backupPath); string(bak) != "current binary" {
		t.Fatalf("backup = %q, want %q", bak, "current binary")
	}
}
