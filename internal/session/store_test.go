package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/session/knownids"
)

func TestSaveAndLoadStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	sessions := []*ManagedSession{
		newSessionWithID("feishu:direct:alice:general", "sess-111"),
		newSessionWithID("feishu:group:xxx:general", "sess-222"),
		{key: "feishu:direct:bob:general"}, // empty session ID should be skipped
	}

	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}

	restored := loadStore(path)
	if len(restored) != 2 {
		t.Fatalf("loadStore() returned %d entries, want 2", len(restored))
	}
	if restored["feishu:direct:alice:general"].SessionID != "sess-111" {
		t.Errorf("alice session = %q, want %q", restored["feishu:direct:alice:general"].SessionID, "sess-111")
	}
	if restored["feishu:group:xxx:general"].SessionID != "sess-222" {
		t.Errorf("group session = %q, want %q", restored["feishu:group:xxx:general"].SessionID, "sess-222")
	}
}

func TestSaveAndLoadUserLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	labeled := newSessionWithID("feishu:direct:alice:general", "sess-111")
	labeled.SetUserLabel("重构会话")
	unlabeled := newSessionWithID("feishu:group:xxx:general", "sess-222")

	sessions := []*ManagedSession{labeled, unlabeled}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}

	restored := loadStore(path)
	if got := restored[labeled.key].UserLabel; got != "重构会话" {
		t.Errorf("labeled UserLabel = %q, want %q", got, "重构会话")
	}
	if got := restored[unlabeled.key].UserLabel; got != "" {
		t.Errorf("unlabeled UserLabel = %q, want empty", got)
	}
}

func TestLoadStoreNotExist(t *testing.T) {
	restored := loadStore("/tmp/does-not-exist-naozhi-test.json")
	if restored != nil {
		t.Errorf("loadStore(nonexistent) = %v, want nil", restored)
	}
}

func TestSaveStoreEmptyPath(t *testing.T) {
	if err := saveStore("", nil); err != nil {
		t.Errorf("saveStore(\"\") error: %v", err)
	}
}

func TestLoadStoreEmptyPath(t *testing.T) {
	if restored := loadStore(""); restored != nil {
		t.Errorf("loadStore(\"\") = %v, want nil", restored)
	}
}

func TestSaveStoreCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "sessions.json")

	sessions := []*ManagedSession{newSessionWithID("test:key", "sess-1")}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestLoadStoreInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("not json"), 0644)

	restored := loadStore(path)
	if restored != nil {
		t.Errorf("loadStore(bad json) = %v, want nil", restored)
	}
}

// corruptSiblingCount returns how many ".corrupt.*" siblings exist for the
// given base path in its directory.
func corruptSiblingCount(t *testing.T, basePath string) int {
	t.Helper()
	matches, err := filepath.Glob(basePath + ".corrupt.*")
	if err != nil {
		t.Fatalf("glob corrupt siblings: %v", err)
	}
	return len(matches)
}

func TestLoadStore_CorruptFilePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadStore(path); got != nil {
		t.Errorf("loadStore(corrupt) = %v, want nil", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("corrupt store should be renamed away from %s, stat err = %v", path, err)
	}
	if n := corruptSiblingCount(t, path); n != 1 {
		t.Errorf("want exactly 1 preserved corrupt sessions.json, got %d", n)
	}
}

// #673: loadWorkspaceOverrides and loadKnownIDs previously discarded a
// corrupt file silently, letting the next atomic save overwrite the bad
// bytes. They now preserve the file the same way loadStore does so a
// partial-write / corruption event leaves a forensic breadcrumb.
func TestLoadWorkspaceOverrides_CorruptFilePreserved(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	ovPath := workspaceOverridesPath(storePath)
	if err := os.WriteFile(ovPath, []byte("][garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadWorkspaceOverrides(storePath); got != nil {
		t.Errorf("loadWorkspaceOverrides(corrupt) = %v, want nil", got)
	}
	if _, err := os.Stat(ovPath); !os.IsNotExist(err) {
		t.Errorf("corrupt overrides should be renamed away, stat err = %v", err)
	}
	if n := corruptSiblingCount(t, ovPath); n != 1 {
		t.Errorf("want exactly 1 preserved corrupt workspace-overrides, got %d", n)
	}
}

func TestLoadKnownIDs_CorruptFilePreserved(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	idsPath := knownIDsPath(storePath)
	if err := os.WriteFile(idsPath, []byte("not an array"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadKnownIDs(storePath); got != nil {
		t.Errorf("loadKnownIDs(corrupt) = %v, want nil", got)
	}
	if _, err := os.Stat(idsPath); !os.IsNotExist(err) {
		t.Errorf("corrupt known-IDs should be renamed away, stat err = %v", err)
	}
	if n := corruptSiblingCount(t, idsPath); n != 1 {
		t.Errorf("want exactly 1 preserved corrupt session-ids, got %d", n)
	}
}

func TestSaveAndLoadPrevSessionIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	s := newSessionWithID("test:key", "sess-3")
	s.prevSessionIDs = []string{"sess-1", "sess-2"}

	sessions := []*ManagedSession{s}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore() error: %v", err)
	}

	restored := loadStore(path)
	if restored == nil {
		t.Fatal("loadStore() returned nil")
	}
	entry := restored["test:key"]
	if entry == nil {
		t.Fatal("entry not found")
	}
	if len(entry.PrevSessionIDs) != 2 || entry.PrevSessionIDs[0] != "sess-1" || entry.PrevSessionIDs[1] != "sess-2" {
		t.Errorf("PrevSessionIDs = %v, want [sess-1 sess-2]", entry.PrevSessionIDs)
	}
}

// TestSaveAndLoadPrevSessionOrigins is the auto-workspace-chain v3
// schema roundtrip: save a session whose chain segments came from
// mixed origins, load it back, ensure the parallel slice survives
// JSON encode/decode.
func TestSaveAndLoadPrevSessionOrigins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	s := newSessionWithID("auto:k", "sess-current")
	s.prevSessionIDs = []string{"sess-1", "sess-2", "sess-3"}
	s.prevSessionOrigins = []string{"manual", "auto-spawn", "auto-backfill"}

	sessions := []*ManagedSession{s}
	if err := saveStore(path, sessions); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	restored := loadStore(path)
	if restored == nil {
		t.Fatal("loadStore returned nil")
	}
	got := restored["auto:k"]
	if got == nil {
		t.Fatal("entry not found")
	}
	want := []string{"manual", "auto-spawn", "auto-backfill"}
	if len(got.PrevSessionOrigins) != len(want) {
		t.Fatalf("PrevSessionOrigins len = %d, want %d (%v)", len(got.PrevSessionOrigins), len(want), got.PrevSessionOrigins)
	}
	for i := range want {
		if got.PrevSessionOrigins[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got.PrevSessionOrigins[i], want[i])
		}
	}
}

// TestLoadStore_LegacyMissingOrigins pins the v3 forward-compat
// guarantee: an on-disk store from a pre-feature naozhi (no
// prev_session_origins field) must still load. ManagedSession
// snapshot defaults the missing tail to "manual".
func TestLoadStore_LegacyMissingOrigins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	legacy := `[{"key":"legacy:k","session_id":"current","prev_session_ids":["a","b"]}]`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatalf("write legacy store: %v", err)
	}

	restored := loadStore(path)
	if restored == nil {
		t.Fatal("loadStore returned nil on legacy schema")
	}
	got := restored["legacy:k"]
	if got == nil {
		t.Fatal("entry not found")
	}
	if len(got.PrevSessionOrigins) != 0 {
		t.Errorf("expected nil PrevSessionOrigins for legacy schema, got %v", got.PrevSessionOrigins)
	}
	// Materialise via ManagedSession snapshot to ensure the "manual"
	// default fires at the read seam.
	s := &ManagedSession{
		key:                got.Key,
		prevSessionIDs:     got.PrevSessionIDs,
		prevSessionOrigins: got.PrevSessionOrigins,
	}
	origins := s.SnapshotPrevSessionOrigins()
	if len(origins) != 2 || origins[0] != "manual" || origins[1] != "manual" {
		t.Errorf("SnapshotPrevSessionOrigins = %v, want [manual manual]", origins)
	}
}

// TestSaveLoadKnownIDs_RoundTripSorted verifies saveKnownIDs (taking the
// pre-sorted SortedSnapshot) round-trips through loadKnownIDs unchanged.
func TestSaveLoadKnownIDs_RoundTripSorted(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")

	var kid knownids.Store
	for _, id := range []string{"zeta", "alpha", "mike"} {
		kid.Track(id)
	}
	sorted := kid.SortedSnapshot()

	if err := saveKnownIDs(storePath, sorted); err != nil {
		t.Fatalf("saveKnownIDs: %v", err)
	}
	loaded := loadKnownIDs(storePath)
	if loaded == nil {
		t.Fatal("loadKnownIDs returned nil")
	}
	for _, id := range []string{"zeta", "alpha", "mike"} {
		if !loaded[id] {
			t.Errorf("loaded set missing %q", id)
		}
	}
	if len(loaded) != 3 {
		t.Errorf("loaded %d IDs, want 3", len(loaded))
	}
}

// TestWorkspaceOverrides_SaveLoadClear covers the round-trip and the
// empty-set removal path (#673): clearing all overrides removes the file and
// fsyncs the parent directory for crash-durability parity with the write
// path. The removal must succeed (and SyncDir must not error) on a normal
// temp dir, and a subsequent load must observe no overrides.
func TestWorkspaceOverrides_SaveLoadClear(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	ovPath := workspaceOverridesPath(storePath)

	overrides := map[string]string{
		"feishu:direct:alice:general": "/home/alice/proj",
		"feishu:group:xxx:general":    "/home/team/repo",
	}
	if err := saveWorkspaceOverrides(storePath, overrides); err != nil {
		t.Fatalf("saveWorkspaceOverrides: %v", err)
	}
	if _, err := os.Stat(ovPath); err != nil {
		t.Fatalf("overrides file missing after save: %v", err)
	}
	got := loadWorkspaceOverrides(storePath)
	if len(got) != 2 || got["feishu:direct:alice:general"] != "/home/alice/proj" {
		t.Fatalf("loadWorkspaceOverrides = %v; want 2 entries round-tripped", got)
	}

	// Clearing to empty must remove the file (and fsync the dir) without error.
	if err := saveWorkspaceOverrides(storePath, map[string]string{}); err != nil {
		t.Fatalf("saveWorkspaceOverrides(empty): %v", err)
	}
	if _, err := os.Stat(ovPath); !os.IsNotExist(err) {
		t.Fatalf("overrides file should be removed after clear; stat err = %v", err)
	}
	if got := loadWorkspaceOverrides(storePath); len(got) != 0 {
		t.Fatalf("loadWorkspaceOverrides after clear = %v; want empty", got)
	}

	// Clearing again (file already absent) must be a no-op, not an error,
	// and must not attempt a spurious dir fsync against a removal that
	// didn't happen.
	if err := saveWorkspaceOverrides(storePath, nil); err != nil {
		t.Fatalf("saveWorkspaceOverrides(nil) on absent file: %v", err)
	}
}

// TestWorkspaceOverrides_ClearRemoveFailurePropagates is the regression guard
// for #2337: when clearing the last override and os.Remove fails with a
// non-ENOENT error (here EACCES, forced via a read-only parent directory), the
// empty-map branch must PROPAGATE the error rather than swallowing it (return
// nil). Returning nil makes the caller clear its dirty flag and never retry,
// leaving the stale file on disk to resurrect the cleared override on the next
// restart via loadWorkspaceOverrides.
func TestWorkspaceOverrides_ClearRemoveFailurePropagates(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses directory write permissions")
	}
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	ovPath := workspaceOverridesPath(storePath)

	// Seed a real overrides file so the clear has something to remove.
	if err := saveWorkspaceOverrides(storePath, map[string]string{
		"feishu:direct:alice:general": "/home/alice/proj",
	}); err != nil {
		t.Fatalf("seed saveWorkspaceOverrides: %v", err)
	}

	// Make the parent directory read-only so os.Remove(ovPath) fails with
	// EACCES (a non-ENOENT error). Restore perms in cleanup so t.TempDir can
	// be removed.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := saveWorkspaceOverrides(storePath, map[string]string{})
	if err == nil {
		t.Fatalf("saveWorkspaceOverrides(empty) swallowed a non-ENOENT remove failure; want error so caller keeps dirty and retries")
	}

	// The stale file must still be on disk — confirming the override would
	// resurrect on restart if the caller had treated the save as successful.
	os.Chmod(dir, 0o700) // re-grant so Stat can traverse
	if _, statErr := os.Stat(ovPath); statErr != nil {
		t.Fatalf("overrides file unexpectedly gone (stat err = %v); test did not exercise the failure path", statErr)
	}
}
