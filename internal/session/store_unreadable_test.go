package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	// Operator moves the oversized file aside. The loaders run once, in
	// NewRouter, so this cannot depend on a second load: the next save tick
	// itself has to notice (#2972).
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}}); err != nil {
		t.Fatalf("save still blocked after the file was moved aside: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("save reported success but wrote nothing: %v", err)
	}
	// Other tests in the package leave their own blocks behind; only this path
	// matters here.
	for _, b := range StoreBlocks() {
		if b.Path == path {
			t.Errorf("StoreBlocks() still lists %s after the lift: %+v", path, b)
		}
	}
}

// A file the operator truncates to 0 bytes is the other "nothing left to
// clobber" shape: jsonfile.Load itself reads an empty file as Absent.
func TestLoadStore_TruncatedFileResumesSaves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	oversizedStoreFile(t, path)
	loadStore(path)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}}); err != nil {
		t.Fatalf("save still blocked after the file was truncated: %v", err)
	}
}

// A file repaired IN PLACE is readable again but was never loaded: the process
// started empty, so a save would still replace the operator's sessions with a
// near-empty set — the #2680 outcome by a longer road. The block has to hold,
// and say what actually lifts it.
func TestLoadStore_RepairedInPlaceStaysBlockedUntilRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	oversizedStoreFile(t, path)
	loadStore(path)

	repaired := []byte(`[{"key":"feishu:direct:u1:general","session_id":"11111111-1111-4111-8111-111111111111"}]`)
	if err := os.WriteFile(path, repaired, 0o600); err != nil {
		t.Fatalf("write repaired: %v", err)
	}
	err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}})
	if err == nil {
		t.Fatal("save went through over a repaired file whose contents were never loaded")
	}
	var blocked *errStoreReadBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("save failed with %v; want errStoreReadBlocked", err)
	}
	if !strings.Contains(err.Error(), "restart naozhi") {
		t.Errorf("reason does not tell the operator what lifts the block: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(repaired) {
		t.Fatalf("repaired file was touched: err=%v body=%s", readErr, got)
	}
	blocks := StoreBlocks()
	found := false
	for _, b := range blocks {
		if b.Path == path {
			found = true
			if !b.NeedRestart {
				t.Errorf("StoreBlocks().NeedRestart = false for a repaired file: %+v", b)
			}
		}
	}
	if !found {
		t.Errorf("StoreBlocks() = %+v, want an entry for %s", blocks, path)
	}
	// A second tick with the file unchanged must not re-read it; the probe is
	// one Lstat. Observable only indirectly: the verdict is the same.
	if err := saveStoreSlice(path, nil); err == nil {
		t.Fatal("second save went through")
	}
	t.Cleanup(func() { storeReadBlocked.Delete(path) })
}

// The 30s save tick used to warn identically on every pass — ~2,880 lines a day
// per blocked file. The first failure warns; the rest are demoted for an hour.
func TestErrStoreReadBlocked_WarnIsThrottled(t *testing.T) {
	b := &storeBlock{label: "session store", reason: "r"}
	e := &errStoreReadBlocked{path: "/x", reason: "r", block: b}
	if !e.warnDue() {
		t.Fatal("first failure must warn")
	}
	if e.warnDue() {
		t.Fatal("second failure within the window must not warn")
	}
	b.mu.Lock()
	b.lastWarn = time.Now().Add(-storeBlockedWarnEvery - time.Second)
	b.mu.Unlock()
	if !e.warnDue() {
		t.Fatal("a failure after the window must warn again")
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
