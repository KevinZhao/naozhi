package session

import (
	"encoding/json"
	"errors"
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
	err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}})
	if err == nil {
		t.Fatal("saveStoreSlice succeeded against an unreadable file; the operator's data was just overwritten")
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

// TestLoadStore_ReadableAgainResumesSaves pins the recovery path: once the
// operator fixes the file, the next successful read lifts the block. Without
// this the process would refuse to persist until restart, turning a temporary
// problem into a permanent one.
func TestLoadStore_ReadableAgainResumesSaves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	oversizedStoreFile(t, path)

	loadStore(path)
	if err := saveStoreSlice(path, nil); err == nil {
		t.Fatal("expected the save to be blocked while the file was unreadable")
	}

	// Operator moves the oversized file aside.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// A read of an absent file is Absent with a nil error: naozhi now owns the
	// path, so writing it destroys nothing.
	loadStore(path)
	if err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}}); err != nil {
		t.Fatalf("save still blocked after the file became readable: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("save reported success but wrote nothing: %v", err)
	}
}

// TestStoreMeta_UnreadableIsRewrittenNotBlocked records the deliberate
// exception, and asserts the part of it that is observable.
//
// The block is keyed by path, so marking the sidecar could never have blocked
// sessions.json — an earlier version of this test claimed to check that and was
// vacuous (a probe that swapped the sidecar onto the blocking path did not fail
// it). The real decision is whether writeStoreMeta consults the guard at all.
// It does not, on purpose: the sidecar is machine-written, a missing one reads
// as legacy, and blocking it would freeze the version number while sessions.json
// keeps advancing — a lost downgrade warning, in exchange for nothing, since
// there is no operator data in it to lose.
//
// So the sidecar must be REWRITTEN over its unreadable contents, and that is
// what this asserts.
func TestStoreMeta_UnreadableIsRewrittenNotBlocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	metaPath := storeMetaPath(path)
	if metaPath == "" {
		t.Skip("no meta sidecar path for this store path")
	}
	oversized := oversizedStoreFile(t, metaPath)

	// loadStore reads the sidecar first; sessions.json itself is absent.
	loadStore(path)
	if err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}}); err != nil {
		t.Fatalf("an unreadable meta sidecar blocked the sessions.json save: %v", err)
	}

	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read back sidecar: %v", err)
	}
	if len(after) == len(oversized) {
		t.Fatalf("sidecar was left at %d bytes; writeStoreMeta must rewrite it", len(after))
	}
	var meta storeMeta
	if err := json.Unmarshal(after, &meta); err != nil {
		t.Fatalf("sidecar is not valid JSON after the save: %v", err)
	}
	if meta.Version != storeFormatVersion {
		t.Errorf("sidecar version = %d, want %d", meta.Version, storeFormatVersion)
	}
}
