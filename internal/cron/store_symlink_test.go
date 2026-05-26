//go:build !windows

package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadJobsRejectsSymlink covers R238-SEC-8 (#829): the cron store
// loader must refuse to follow a symlink at the configured store path.
// Previously the Lstat → os.Open shape left a TOCTOU window in which a
// local attacker who could write the data dir could swap cron_jobs.json
// for a symlink between the two syscalls; openCronStoreFile uses
// O_NOFOLLOW so the kernel refuses the follow atomically and Fstat on
// the resulting fd validates the inode is a regular file.
//
// The test seeds a real cron_jobs.json at a "secret" path, then exposes
// it via a symlink the loader is asked to read. A correct implementation
// returns the "is a symlink, refusing to follow" error and never reads
// the secret bytes.
func TestLoadJobsRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.json")
	link := filepath.Join(dir, "cron_jobs.json")

	if err := os.WriteFile(secret, []byte(`[{"id":"abcd1234abcd1234","prompt":"x","schedule":"@every 1m"}]`), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	m, err := loadJobs(link)
	if err == nil {
		t.Fatalf("expected error refusing symlink, got nil err with map=%v", m)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected error to mention symlink, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil map on symlink, got %d entries", len(m))
	}
}

// TestLoadJobsRejectsDirectory covers the Fstat IsRegular guard: even
// when O_NOFOLLOW opens succeed on non-regular paths, Fstat on the fd
// must catch them before json.Unmarshal sees the bytes. A directory at
// the cron store path is the simplest non-regular case to exercise on
// any unix without needing CAP_MKNOD/mkfifo support — open() returns a
// directory fd, our Fstat IsRegular check rejects it.
func TestLoadJobsRejectsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	m, err := loadJobs(path)
	if err == nil {
		t.Fatalf("expected error rejecting directory, got nil err with map=%v", m)
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("expected error to mention regular file, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil map on directory, got %d entries", len(m))
	}
}
