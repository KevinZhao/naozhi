package server

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestPublicTmpFileForbidden_OwnerOnlyOtherUID locks the R245-SEC-7 (#831)
// gate. The __public_tmp__ pseudo-project lets any authenticated dashboard
// caller read /tmp; on a multi-tenant host another UNIX user could leave a
// 0600 file there. The dashboard process runs as the operator's UID and
// can still read+stat those files (operator might have CAP_DAC_READ_SEARCH
// or share a group), so without this gate a stolen dashboard token would
// gain enumeration of every user's /tmp dump on the host.
//
// We construct a synthetic os.FileInfo via a real file we own first to
// confirm the allow path, then via a swapped-stat to confirm the deny path.
// Constructing a foreign-UID file from a non-root test runner is not
// possible, so we exercise the deny branch by feeding a stub FileInfo
// whose Stat_t.Uid mismatches os.Geteuid().
func TestPublicTmpFileForbidden_OwnerOnlyOtherUID(t *testing.T) {
	dir := t.TempDir()
	owned := filepath.Join(dir, "owned-0600")
	if err := os.WriteFile(owned, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	infoOwned, err := os.Lstat(owned)
	if err != nil {
		t.Fatal(err)
	}
	if publicTmpFileForbidden(infoOwned) {
		t.Errorf("0600 file owned by current UID should be allowed; got refused")
	}

	worldRead := filepath.Join(dir, "world-0644")
	if err := os.WriteFile(worldRead, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	infoWorld, err := os.Lstat(worldRead)
	if err != nil {
		t.Fatal(err)
	}
	if publicTmpFileForbidden(infoWorld) {
		t.Errorf("0644 file should be allowed (already group/world readable so the gate adds no protection); got refused")
	}

	groupRead := filepath.Join(dir, "group-0640")
	if err := os.WriteFile(groupRead, []byte("group"), 0o640); err != nil {
		t.Fatal(err)
	}
	infoGroup, err := os.Lstat(groupRead)
	if err != nil {
		t.Fatal(err)
	}
	if publicTmpFileForbidden(infoGroup) {
		t.Errorf("0640 file should be allowed (group-readable already); got refused")
	}
}

// TestPublicTmpFileForbidden_ForeignUIDStub exercises the deny branch by
// supplying a fakeFileInfo whose Stat_t.Uid is deliberately != Geteuid().
// We can't chown a real file from a non-root test, so we stub at the
// FileInfo seam. The stub is local to this test so the production path
// keeps using real os.Lstat output.
func TestPublicTmpFileForbidden_ForeignUIDStub(t *testing.T) {
	euid := uint32(os.Geteuid())
	// Pick a UID guaranteed-different from current. 0 (root) when we are not
	// root; otherwise euid+1 (which on root tests is 1, also a real UID
	// nobody-owned).
	foreignUID := uint32(0)
	if euid == 0 {
		foreignUID = 1
	}

	stub := stubFileInfo{
		mode: 0o600,
		st:   &syscall.Stat_t{Uid: foreignUID},
	}
	if !publicTmpFileForbidden(stub) {
		t.Errorf("0600 file owned by UID %d should be refused (current euid=%d)",
			foreignUID, euid)
	}

	// Same UID-mismatch but mode 0o644 should be allowed: the file is
	// already group/world-readable so the dashboard route adds no
	// new disclosure.
	stub.mode = 0o644
	if publicTmpFileForbidden(stub) {
		t.Errorf("0644 file with foreign UID should be allowed (already public); got refused")
	}
}

// stubFileInfo implements os.FileInfo with a synthetic *syscall.Stat_t so
// we can drive publicTmpFileForbidden's UID branch from a non-root test.
// Only the methods publicTmpFileForbidden actually calls are meaningful;
// the rest return zero values.
type stubFileInfo struct {
	mode os.FileMode
	st   *syscall.Stat_t
}

func (s stubFileInfo) Name() string       { return "stub" }
func (s stubFileInfo) Size() int64        { return 0 }
func (s stubFileInfo) Mode() os.FileMode  { return s.mode }
func (s stubFileInfo) ModTime() time.Time { return time.Time{} }
func (s stubFileInfo) IsDir() bool        { return false }
func (s stubFileInfo) Sys() any           { return s.st }
