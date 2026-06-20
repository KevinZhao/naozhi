package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveKnownIDs_EnsureDirGuarded pins [R20260614-GO-003]: saveKnownIDs must
// use the storeDirEnsured sync.Map to skip the MkdirAll+Lstat+Chmod syscalls
// on the second and subsequent calls for the same directory. We verify this by
// observing that: (a) the first call succeeds and creates the file, and (b)
// the second call with an already-recorded dir also succeeds without needing
// the directory to be freshly creatable.
func TestSaveKnownIDs_EnsureDirGuarded(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")

	// Prime storeDirEnsured as writeStoreData would — simulating a prior
	// successful writeStoreData call for the same directory.
	dir := filepath.Dir(knownIDsPath(storePath))
	if dir == "" {
		t.Skip("knownIDsPath returned empty dir")
	}
	// Ensure dir exists (as writeStoreData would have done it).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	// Record it as already ensured.
	storeDirEnsured.Store(dir, struct{}{})
	t.Cleanup(func() { storeDirEnsured.Delete(dir) })

	ids := []string{"alpha", "beta"}

	// First call: dir is already ensured, should succeed.
	if err := saveKnownIDs(storePath, ids); err != nil {
		t.Fatalf("first saveKnownIDs: %v", err)
	}

	// Second call (simulating the next tick): still succeeds.
	if err := saveKnownIDs(storePath, ids); err != nil {
		t.Fatalf("second saveKnownIDs: %v", err)
	}

	// Verify the file was actually written.
	path := knownIDsPath(storePath)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("known IDs file missing after save: %v", err)
	}
}

// TestSaveKnownIDs_EnsureDirRecordedOnFirstSave verifies that saveKnownIDs
// records the directory in storeDirEnsured on the first successful call, so
// subsequent calls (including from writeStoreData) see it as already done.
func TestSaveKnownIDs_EnsureDirRecordedOnFirstSave(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sub", "sessions.json")

	dir := filepath.Dir(knownIDsPath(storePath))
	if dir == "" {
		t.Skip("knownIDsPath returned empty dir")
	}
	// Ensure the dir does not appear in storeDirEnsured yet.
	storeDirEnsured.Delete(dir)
	t.Cleanup(func() { storeDirEnsured.Delete(dir) })

	ids := []string{"id1"}
	if err := saveKnownIDs(storePath, ids); err != nil {
		t.Fatalf("saveKnownIDs: %v", err)
	}

	// After a successful call the dir must be recorded.
	if _, ok := storeDirEnsured.Load(dir); !ok {
		t.Errorf("storeDirEnsured not set for %q after successful saveKnownIDs", dir)
	}
}

// TestWriteStoreData_MetaWrittenOnce pins [R20260614-PERF-005]: writeStoreData
// must call writeStoreMeta only on the first successful write per path; on
// subsequent calls the meta sidecar file must NOT be re-written (mtime must
// not advance). We observe this via the mtime of the sidecar file.
func TestWriteStoreData_MetaWrittenOnce(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")

	// Clear any residual state from parallel tests.
	storeMetaWritten.Delete(storePath)
	storeDirEnsured.Delete(filepath.Dir(storePath))
	t.Cleanup(func() {
		storeMetaWritten.Delete(storePath)
		storeDirEnsured.Delete(filepath.Dir(storePath))
	})

	data := []byte(`{"v":1}`)

	// First write: meta sidecar should be created.
	if err := writeStoreData(storePath, data); err != nil {
		t.Fatalf("first writeStoreData: %v", err)
	}
	metaPath := storeMetaPath(storePath)
	if metaPath == "" {
		t.Skip("storeMetaPath returned empty; cannot check mtime")
	}
	info1, err := os.Stat(metaPath)
	if err != nil {
		t.Fatalf("meta file missing after first write: %v", err)
	}
	mtime1 := info1.ModTime()

	// Verify storeMetaWritten is now set.
	if _, ok := storeMetaWritten.Load(storePath); !ok {
		t.Error("storeMetaWritten not set after first writeStoreData")
	}

	// Sleep a tiny bit so any FS timestamp resolution can show a difference
	// if writeStoreMeta were called again.
	// We use a stat-based approach: overwrite the sidecar with a sentinel,
	// then call writeStoreData again and verify the sentinel survives.
	sentinel := []byte(`{"sentinel":true}`)
	if err := os.WriteFile(metaPath, sentinel, 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Second write: writeStoreMeta must NOT be called again.
	if err := writeStoreData(storePath, data); err != nil {
		t.Fatalf("second writeStoreData: %v", err)
	}

	got, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta after second write: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("meta file was overwritten on second writeStoreData call; "+
			"storeMetaWritten guard not effective.\n"+
			"first mtime=%v, content after second write=%q (want sentinel %q)",
			mtime1, got, sentinel)
	}
}
