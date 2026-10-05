package project

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
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

// gitIn runs git in dir with the host's global and system config ignored.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
}

// A stub git tracks, in the project's own repo or in a repo around the
// projects root, is kept, so the sweep leaves no deletion in the working
// tree; an untracked stub in a repo still goes. GIT_DIR and GIT_INDEX_FILE
// naming another repo do not reach the probe.
func TestScan_LegacyStubTrackedByGitKept(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	tracked := writeStub(t, root, "tracked", 100)
	gitIn(t, filepath.Join(root, "tracked"), "init", "-q")
	gitIn(t, filepath.Join(root, "tracked"), "add", "--", ".naozhi/project.yaml")
	untracked := writeStub(t, root, "untracked", 200)
	gitIn(t, filepath.Join(root, "untracked"), "init", "-q")
	inParent := writeStub(t, root, "inparent", 300)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", "--", "inparent/.naozhi/project.yaml")
	other := t.TempDir()
	gitIn(t, other, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")

	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	assertExists(t, tracked, true)
	assertExists(t, inParent, true)
	assertExists(t, untracked, false)
	assertExists(t, filepath.Dir(untracked), false)
	if !slices.Equal(readIndexFile(t, indexPath).StubCleanupDone, []string{root}) {
		t.Error("sweep not recorded as done")
	}
}

// When git cannot answer, a stub is kept only if its project lies in a git
// checkout: its own .git (a repo directory or a worktree's gitdir file) or a
// parent's. Git is not asked about a project outside any checkout.
func TestScan_LegacyStubUnknownTrackingKeptInRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := writeStub(t, root, "repo", 1)
	if err := os.Mkdir(filepath.Join(root, "repo", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	worktree := writeStub(t, root, "worktree", 2)
	if err := os.WriteFile(filepath.Join(root, "worktree", ".git"), []byte("gitdir: /elsewhere\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plain := writeStub(t, root, "plain", 3)
	var probed []string
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(filepath.Join(t.TempDir(), "projects-index.json")))
	m.stubProbe = func(dir string) trackState {
		probed = append(probed, filepath.Base(dir))
		return trackUnknown
	}
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	assertExists(t, repo, true)
	assertExists(t, worktree, true)
	assertExists(t, plain, false)
	slices.Sort(probed)
	if !slices.Equal(probed, []string{"repo", "worktree"}) {
		t.Errorf("git probed %q; want only the projects in a checkout", probed)
	}

	t.Run("parent repo", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "checkout", "projects")
		child := writeStub(t, root, "child", 4)
		if err := os.Mkdir(filepath.Join(filepath.Dir(root), ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(filepath.Join(t.TempDir(), "projects-index.json")))
		m.stubProbe = func(string) trackState { return trackUnknown }
		if err := m.Scan(); err != nil {
			t.Fatal(err)
		}
		assertExists(t, child, true)
	})
}

// The git probe runs without m.mu, and the removal that follows waits for a
// writer holding m.mu and re-checks the bytes, so the writer's save is kept.
func TestSweepLegacyStubs_ProbeUnlockedRemoveRechecks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stub := writeStub(t, root, "proj", 100)
	if err := os.Mkdir(filepath.Join(root, "proj", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(filepath.Join(t.TempDir(), "projects-index.json")))
	writerHolds, release := make(chan struct{}), make(chan struct{})
	probeHeldMu := false // read only after Scan returns
	m.stubProbe = func(string) trackState {
		if probeHeldMu = !m.mu.TryLock(); !probeHeldMu {
			m.mu.Unlock()
		}
		go func() { // a writer mid-save, started while the sweep probes git
			m.mu.Lock()
			close(writerHolds)
			<-release
			m.mu.Unlock()
		}()
		select {
		case <-writerHolds:
		case <-time.After(time.Second):
		}
		return trackUntracked
	}
	done := make(chan error, 1)
	go func() { done <- m.Scan() }()
	select {
	case <-writerHolds:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("writer never got m.mu")
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("sweep finished (err %v) while a writer held m.mu", err)
	case <-time.After(100 * time.Millisecond):
	}
	assertExists(t, stub, true)
	saved := []byte("created_at: 100\nfavorite: true\n")
	if err := os.WriteFile(stub, saved, 0600); err != nil {
		close(release)
		t.Fatalf("writer save: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if probeHeldMu {
		t.Error("the git probe ran under m.mu")
	}
	if got, err := os.ReadFile(stub); err != nil || string(got) != string(saved) {
		t.Errorf("writer's project.yaml = %q, %v; want it kept as %q", got, err, saved)
	}
}
