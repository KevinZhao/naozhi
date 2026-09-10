package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStorePathsFollowTheConfiguredFilename is the regression that made the old
// datadir API unusable (#2641). `session.store_path` is an operator-configured
// FILE path, not a fixed name under a data root, so every sibling this package
// derives has to hang off the configured directory while the store file itself
// keeps the configured name. A layout helper that rebuilt "<root>/sessions.json"
// would silently point the loader at a file that does not exist.
func TestStorePathsFollowTheConfiguredFilename(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "mystore.json")

	if got, want := storeMetaPath(store), filepath.Join(dir, "mystore.meta.json"); got != want {
		t.Errorf("storeMetaPath = %q, want %q — the sidecar name must follow the configured file", got, want)
	}
	if got, want := knownIDsPath(store), filepath.Join(dir, "session-ids.json"); got != want {
		t.Errorf("knownIDsPath = %q, want %q", got, want)
	}
	if got, want := workspaceOverridesPath(store), filepath.Join(dir, "workspace-overrides.json"); got != want {
		t.Errorf("workspaceOverridesPath = %q, want %q", got, want)
	}

	// And the write actually lands on the configured name, not on sessions.json.
	if err := saveStore(store, map[string]*ManagedSession{}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	if _, err := os.Stat(store); err != nil {
		t.Fatalf("configured store file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err == nil {
		t.Error("wrote sessions.json instead of the configured filename")
	}
}

// TestStorePathsEmptyStoreStaysEmpty: an unset store path must not turn the
// sidecars into writes at the filesystem root. Each of these guarded on
// storePath == "" before the layout was introduced and must still do so.
func TestStorePathsEmptyStoreStaysEmpty(t *testing.T) {
	if got := storeMetaPath(""); got != "" {
		t.Errorf("storeMetaPath(\"\") = %q, want empty", got)
	}
	if got := knownIDsPath(""); got != "" {
		t.Errorf("knownIDsPath(\"\") = %q, want empty", got)
	}
	if got := workspaceOverridesPath(""); got != "" {
		t.Errorf("workspaceOverridesPath(\"\") = %q, want empty", got)
	}
}
