package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// oversizedStoreFile writes a file just past maxStoreFileBytes so jsonfile.Load
// refuses it with a non-nil error while leaving it on disk. Returns the bytes it
// wrote so a caller can prove the file is untouched later.
func oversizedStoreFile(t *testing.T, path string) []byte {
	t.Helper()
	// Recognisable content, not zeroes: a byte-for-byte comparison against a
	// rewritten file has to fail for a reason other than length.
	body := make([]byte, maxStoreFileBytes+1)
	for i := range body {
		body[i] = byte('A' + i%26)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write oversized %s: %v", path, err)
	}
	return body
}

// TestLoadStore_OversizedFileIsNotOverwritten is the #2680 acceptance case: an
// operator's sessions.json that is over the size cap must survive a save.
//
// jsonfile.Load's godoc already states the rule — "Callers MUST NOT continue
// with empty state in that case: the next atomic save would clobber the real
// file" — and loadStore used to do exactly that: warn, return nil, start empty.
// The next WriteFileAtomic then replaced 4 MiB of the operator's data with a
// fresh two-session file.
func TestLoadStore_OversizedFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	original := oversizedStoreFile(t, path)

	// The read fails and reports nil, exactly as before — starting empty is
	// still the right call for availability. What changes is what happens next.
	if got := loadStore(path); got != nil {
		t.Fatalf("loadStore returned %d entries for an oversized file; want nil", len(got))
	}

	// Any save attempt must refuse rather than write.
	err := saveStore(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}})
	if err == nil {
		t.Fatal("saveStore succeeded against an unreadable file; the operator's data was just overwritten")
	}
	var blocked *errStoreReadBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("save failed with %v; want errStoreReadBlocked so callers keep the dirty flag set", err)
	}

	// The bytes on disk are the operator's, unchanged.
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back %s: %v", path, readErr)
	}
	if len(after) != len(original) {
		t.Fatalf("file length changed: %d -> %d", len(original), len(after))
	}
	for i := range original {
		if after[i] != original[i] {
			t.Fatalf("file contents changed at byte %d", i)
		}
	}
}

// TestKnownIDsAndOverrides_OversizedFilesAreNotOverwritten covers the other two
// writable store files. They carry less than sessions.json but the same rule
// applies: naozhi holds no copy of what is there.
func TestKnownIDsAndOverrides_OversizedFilesAreNotOverwritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		file func(storePath string) string
		load func(storePath string)
		save func(storePath string) error
	}{
		{
			name: "known session IDs",
			file: knownIDsPath,
			load: func(sp string) { loadKnownIDs(sp) },
			save: func(sp string) error { return saveKnownIDsBytes(sp, []byte(`["abc"]`)) },
		},
		{
			name: "workspace overrides",
			file: workspaceOverridesPath,
			load: func(sp string) { loadWorkspaceOverrides(sp) },
			save: func(sp string) error { return saveWorkspaceOverrides(sp, map[string]string{"k": "/tmp"}) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			storePath := filepath.Join(dir, "sessions.json")
			target := tc.file(storePath)
			original := oversizedStoreFile(t, target)

			tc.load(storePath)
			if err := tc.save(storePath); err == nil {
				t.Fatalf("save succeeded against an unreadable %s", tc.name)
			}
			after, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if len(after) != len(original) {
				t.Fatalf("%s length changed: %d -> %d", tc.name, len(original), len(after))
			}
		})
	}
}

// A symlinked sessions.json is refused by jsonfile.Load, so the process starts
// without its contents; the save that follows must neither replace the link
// (WriteFileAtomic's rename would) nor write through it to the target.
func TestLoadStore_SymlinkedFileIsNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	target := filepath.Join(t.TempDir(), "real-sessions.json")
	body := []byte(`[{"key":"feishu:direct:u1:general","session_id":"11111111-1111-4111-8111-111111111111"}]`)
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Cleanup(func() { storeReadBlocked.Delete(path) })

	if got := loadStore(path); got != nil {
		t.Fatalf("loadStore followed the symlink and loaded %d entries", len(got))
	}
	var blocked *errStoreReadBlocked
	if err := saveStore(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}}); !errors.As(err, &blocked) {
		t.Fatalf("save over a symlinked store returned %v; want errStoreReadBlocked", err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("sessions.json is no longer a symlink: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(body) {
		t.Errorf("symlink target was rewritten: %s", got)
	}
}

// StoreWriteBlocks is the Router-scoped view /health serves: this Router's own
// store files, not every blocked path in the process.
func TestRouter_StoreWriteBlocks_ScopedToOwnFiles(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	oversizedStoreFile(t, storePath)
	other := filepath.Join(t.TempDir(), "sessions.json")
	oversizedStoreFile(t, other)
	loadStore(other)
	t.Cleanup(func() { storeReadBlocked.Delete(other) })

	r := NewRouter(RouterConfig{MaxProcs: 1, StorePath: storePath})
	t.Cleanup(func() { storeReadBlocked.Delete(storePath) })
	t.Cleanup(r.Shutdown)
	blocks := r.StoreWriteBlocks()
	if len(blocks) != 1 || blocks[0].Path != storePath || blocks[0].Label != "session store" {
		t.Fatalf("StoreWriteBlocks() = %+v, want exactly this router's sessions.json", blocks)
	}
	if blocks[0].Reason == "" || blocks[0].Since.IsZero() {
		t.Errorf("block lacks reason/since: %+v", blocks[0])
	}
	var nilRouter *Router
	if got := nilRouter.StoreWriteBlocks(); got != nil {
		t.Errorf("nil router reported %+v", got)
	}
}
