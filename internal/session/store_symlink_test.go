package session

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// symlinkedStoreFile points path at a regular file holding body in another
// directory and returns the target path. Skips where symlinks are unavailable.
func symlinkedStoreFile(t *testing.T, path string, body []byte) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "real-"+filepath.Base(path))
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return target
}

// captureStoreLog routes slog to a buffer for the rest of the test. Not safe
// alongside t.Parallel: slog.SetDefault is process-global.
func captureStoreLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// assertSymlinkIntact checks that path is still a symlink and its target still
// holds want: a save must neither replace the link nor write through it.
func assertSymlinkIntact(t *testing.T, path, target string, want []byte) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink (mode %v); the save replaced it", path, fi.Mode())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("symlink target was rewritten: %s", got)
	}
}

// A symlinked sessions.json is refused by jsonfile.Load, so the process starts
// without its contents. The save that follows must not replace the link (which
// is what WriteFileAtomic's rename would do), and the operator has to be told
// it is a symlink rather than a damaged file.
func TestLoadStore_SymlinkedFileBlocksSavesWithSymlinkHint(t *testing.T) {
	logs := captureStoreLog(t)
	path := filepath.Join(t.TempDir(), "sessions.json")
	body := []byte(`[{"key":"feishu:direct:u1:general","session_id":"11111111-1111-4111-8111-111111111111"}]`)
	target := symlinkedStoreFile(t, path, body)
	t.Cleanup(func() { storeReadBlocked.Delete(path) })

	if got := loadStore(path); got != nil {
		t.Fatalf("loadStore followed the symlink and loaded %d entries", len(got))
	}
	err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}})
	var blocked *errStoreReadBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("save over a symlinked store returned %v; want errStoreReadBlocked", err)
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("block reason does not name the symlink: %v", err)
	}
	assertSymlinkIntact(t, path, target, body)

	out := logs.String()
	if !strings.Contains(out, "replace the symlink with the real file") {
		t.Errorf("operator hint does not say what to do about the symlink:\n%s", out)
	}
	if strings.Contains(out, "fix or move the file aside") {
		t.Errorf("symlink got the generic unreadable-file hint:\n%s", out)
	}
}

// The other two writable store files go through the same guard.
func TestKnownIDsAndOverrides_SymlinkedFilesAreNotReplaced(t *testing.T) {
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
			storePath := filepath.Join(t.TempDir(), "sessions.json")
			path := tc.file(storePath)
			body := []byte(`{}`)
			target := symlinkedStoreFile(t, path, body)
			t.Cleanup(func() { storeReadBlocked.Delete(path) })

			tc.load(storePath)
			err := tc.save(storePath)
			if err == nil || !strings.Contains(err.Error(), "is a symlink") {
				t.Fatalf("save over a symlinked %s returned %v; want a symlink block", tc.name, err)
			}
			assertSymlinkIntact(t, path, target, body)
		})
	}
}

// What the operator does about the symlink decides whether the block lifts on
// the next save tick: removing it, or leaving an empty regular file, leaves
// nothing to clobber; putting the real file in its place does not, because
// naozhi started without those contents.
func TestLoadStore_SymlinkReplacedLiftsOnlyWhenNothingToClobber(t *testing.T) {
	for _, tc := range []struct {
		name      string
		replace   func(path string) error
		wantLifts bool
	}{
		{name: "removed", replace: os.Remove, wantLifts: true},
		{name: "empty regular file", replace: func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.WriteFile(p, nil, 0o600)
		}, wantLifts: true},
		{name: "real file moved into place", replace: func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.WriteFile(p, []byte(`[]`), 0o600)
		}, wantLifts: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.json")
			symlinkedStoreFile(t, path, []byte(`[]`))
			t.Cleanup(func() { storeReadBlocked.Delete(path) })
			loadStore(path)
			if err := saveStoreSlice(path, nil); err == nil {
				t.Fatal("save went through while sessions.json was a symlink")
			}

			if err := tc.replace(path); err != nil {
				t.Fatalf("replace symlink: %v", err)
			}
			err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}})
			if tc.wantLifts {
				if err != nil {
					t.Fatalf("save still blocked after the symlink was replaced: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "restart naozhi") {
				t.Fatalf("save over an unloaded real file returned %v; want a block naming the restart", err)
			}
		})
	}
}

// The meta sidecar is report-only: a symlinked one does not stop sessions.json
// from saving, and the sidecar write replaces the link without writing through
// it to the target.
func TestStoreMeta_SymlinkedIsReportedNotBlocked(t *testing.T) {
	logs := captureStoreLog(t)
	path := filepath.Join(t.TempDir(), "sessions.json")
	metaPath := storeMetaPath(path)
	if metaPath == "" {
		t.Skip("no meta sidecar path for this store path")
	}
	body := []byte(`{"version":1}`)
	target := symlinkedStoreFile(t, metaPath, body)

	loadStore(path)
	if err := saveStoreSlice(path, []*ManagedSession{{key: "dashboard:direct:abc:general"}}); err != nil {
		t.Fatalf("a symlinked meta sidecar blocked the sessions.json save: %v", err)
	}
	fi, err := os.Lstat(metaPath)
	if err != nil {
		t.Fatalf("lstat sidecar: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("sidecar mode = %v after the save; want a regular file", fi.Mode())
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, body) {
		t.Errorf("save wrote through the symlink to its target: %s", got)
	}
	if out := logs.String(); !strings.Contains(out, "symlink target is left untouched") {
		t.Errorf("report-only warning does not explain the symlink:\n%s", out)
	}
}
