package project

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeStub writes the exact project.yaml an earlier Scan generated.
func writeStub(t *testing.T, root, name string, createdAt int64) string {
	t.Helper()
	makeProjectDir(t, root, name, &ProjectConfig{CreatedAt: createdAt})
	return filepath.Join(root, name, configDir, configFile)
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if got := err == nil; got != want {
		t.Errorf("%s exists = %v, want %v (err %v)", path, got, want, err)
	}
}

// A legacy stub is removed together with its .naozhi/, its CreatedAt moves to
// the index first, and a fresh Manager keeps the old order.
func TestScan_LegacyStubRemovedOrderKept(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stubB := writeStub(t, root, "b", 100)
	stubA := writeStub(t, root, "a", 200)
	makeProjectDir(t, root, "c", nil)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")

	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	for _, stub := range []string{stubA, stubB} {
		assertExists(t, stub, false)
		assertExists(t, filepath.Dir(stub), false)
	}
	f := readIndexFile(t, indexPath)
	if a, b := f.CreatedAt[filepath.Join(root, "a")], f.CreatedAt[filepath.Join(root, "b")]; a != 200 || b != 100 {
		t.Errorf("index a=%d b=%d, want the stubs' 200 and 100", a, b)
	}
	if !slices.Equal(f.StubCleanupDone, []string{root}) {
		t.Errorf("stub_cleanup_done = %q, want [%q]", f.StubCleanupDone, root)
	}

	fresh, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := fresh.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := projectNames(fresh.All()); got != "b,a,c" {
		t.Errorf("order after restart = %s, want b,a,c", got)
	}
	if got := createdAtOf(t, fresh, "a"); got != 200 {
		t.Errorf("a CreatedAt after restart = %d, want 200", got)
	}
}

// Only a byte-exact stub that is alone in .naozhi/ goes; user content,
// siblings, symlinks and the include_root project are left untouched.
func TestScan_LegacyStubLookalikesUntouched(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	makeProjectDir(t, root, "fav", &ProjectConfig{CreatedAt: 1, Favorite: true})

	commented := writeStub(t, root, "commented", 2)
	if err := os.WriteFile(commented, []byte("# keep\ncreated_at: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Same length as the stub "created_at: 20\n", so only the bytes differ.
	spaced := writeStub(t, root, "spaced", 20)
	if err := os.WriteFile(spaced, []byte("created_at:  20"), 0600); err != nil {
		t.Fatal(err)
	}
	sibling := writeStub(t, root, "sibling", 3)
	if err := os.WriteFile(filepath.Join(filepath.Dir(sibling), "settings.json"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// The link text is as long as the stub, so only the file type differs.
	linked := writeStub(t, root, "linked", 4)
	if err := os.Rename(linked, filepath.Join(root, "linked", "target.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target.yaml", linked); err != nil {
		t.Fatal(err)
	}
	// A symlinked .naozhi/ whose target holds a byte-exact stub.
	shared := filepath.Join(parent, "shared")
	if err := saveConfigToPath(filepath.Join(shared, configFile), ProjectConfig{CreatedAt: 6}); err != nil {
		t.Fatal(err)
	}
	makeProjectDir(t, root, "dirlinked", nil)
	if err := os.Symlink(shared, filepath.Join(root, "dirlinked", configDir)); err != nil {
		t.Fatal(err)
	}
	rootStub := filepath.Join(root, configDir, configFile)
	if err := saveConfigToPath(rootStub, ProjectConfig{CreatedAt: 5}); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	// An index entry equal to the root's yaml value must not make it a stub.
	writeIndexFile(t, indexPath, map[string]int64{root: 5})
	before := treeSnapshot(t, root)
	maps.Copy(before, treeSnapshot(t, shared))

	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath), WithIncludeRoot(true))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	after := treeSnapshot(t, root)
	maps.Copy(after, treeSnapshot(t, shared))
	for path, v := range before {
		if after[path] != v {
			t.Errorf("Scan changed %s: before %q, after %q", path, v, after[path])
		}
	}
	if !slices.Equal(readIndexFile(t, indexPath).StubCleanupDone, []string{root}) {
		t.Error("sweep not recorded as done")
	}
}

// After the one-shot sweep a stub the user restores (or commits) survives
// every later scan, including a fresh Manager's.
func TestScan_LegacyStubRestoredAfterSweepSurvives(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stub := writeStub(t, root, "p", 100)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stub, false)

	writeStub(t, root, "p", 100)
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stub, true)
	fresh, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := fresh.Scan(); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stub, true)
}

// The sweep is recorded per root: a root that was swept does not stop the
// first scan of another root from sweeping its own stubs.
func TestScan_LegacyStubSweepPerRoot(t *testing.T) {
	t.Parallel()
	rootA, rootB := t.TempDir(), t.TempDir()
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	mA, _ := NewManager(rootA, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := mA.Scan(); err != nil {
		t.Fatal(err)
	}
	stub := writeStub(t, rootB, "p", 100)
	mB, _ := NewManager(rootB, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := mB.Scan(); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stub, false)
	got := readIndexFile(t, indexPath).StubCleanupDone
	if want := slices.Sorted(slices.Values([]string{rootA, rootB})); !slices.Equal(got, want) {
		t.Errorf("stub_cleanup_done = %q, want %q", got, want)
	}
}

// A stub is only removed once its CreatedAt is on disk elsewhere: with no
// index file, an unusable one, or a failing save it stays, and the sweep runs
// once a save succeeds.
func TestScan_LegacyStubKeptUntilIndexDurable(t *testing.T) {
	t.Parallel()
	t.Run("in-memory", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		stub := writeStub(t, root, "p", 100)
		m, _ := NewManager(root, PlannerDefaults{})
		if err := m.Scan(); err != nil {
			t.Fatal(err)
		}
		assertExists(t, stub, true)
	})
	t.Run("newer-version", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		stub := writeStub(t, root, "p", 100)
		indexPath := filepath.Join(t.TempDir(), "projects-index.json")
		// It already holds p's order, but this build may not rely on it.
		newer := fmt.Sprintf(`{"version":99,"created_at":{%q:100}}`, filepath.Join(root, "p"))
		if err := os.WriteFile(indexPath, []byte(newer), 0600); err != nil {
			t.Fatal(err)
		}
		m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
		if err := m.Scan(); err != nil {
			t.Fatal(err)
		}
		assertExists(t, stub, true)
	})
	t.Run("save-failing", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		stub := writeStub(t, root, "p", 100)
		stateDir := filepath.Join(t.TempDir(), "state")
		indexPath := filepath.Join(stateDir, "projects-index.json")
		m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
		// A file where the state dir should be makes the first save fail.
		if err := os.WriteFile(stateDir, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := m.Scan(); err != nil {
			t.Fatal(err)
		}
		assertExists(t, stub, true)
		if err := os.Remove(stateDir); err != nil {
			t.Fatal(err)
		}
		if err := m.Scan(); err != nil {
			t.Fatal(err)
		}
		assertExists(t, stub, false)
		if got := readIndexFile(t, indexPath).CreatedAt[filepath.Join(root, "p")]; got != 100 {
			t.Errorf("index[p] = %d, want the stub's 100", got)
		}
	})
}
