package project

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// treeSnapshot maps every path under root to its mode and (for files) bytes.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			v += ":" + string(data)
		}
		snap[path] = v
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return snap
}

func readIndexFile(t *testing.T, path string) indexFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var f indexFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse index %q: %v", data, err)
	}
	return f
}

func writeIndexFile(t *testing.T, path string, createdAt map[string]int64) {
	t.Helper()
	data, err := json.Marshal(indexFile{Version: projectsIndexVersion, CreatedAt: createdAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func createdAtOf(t *testing.T, m *Manager, name string) int64 {
	t.Helper()
	p := m.Get(name)
	if p == nil {
		t.Fatalf("project %q missing", name)
	}
	return p.Config.CreatedAt
}

// Scan is read-only on projects.root: bare dirs, git repos and existing
// project.yaml files (with or without created_at) stay byte-identical, and
// the stamped order goes to the index instead.
func TestScan_DoesNotWriteIntoProjectDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "bare", nil)
	makeProjectDir(t, root, "stamped", &ProjectConfig{CreatedAt: 100})
	makeProjectDir(t, root, "unstamped", &ProjectConfig{Favorite: true})
	makeProjectDir(t, root, "repo", nil)
	if err := os.MkdirAll(filepath.Join(root, "repo", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo", ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://github.com/o/r.git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, root)

	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	for i := 0; i < 2; i++ {
		if err := m.Scan(); err != nil {
			t.Fatalf("Scan #%d: %v", i, err)
		}
	}

	after := treeSnapshot(t, root)
	for path, v := range after {
		if before[path] != v {
			t.Errorf("Scan changed %s: before %q, after %q", path, before[path], v)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("Scan removed %s", path)
		}
	}

	f := readIndexFile(t, indexPath)
	if f.Version != projectsIndexVersion {
		t.Errorf("index version = %d, want %d", f.Version, projectsIndexVersion)
	}
	for _, name := range []string{"bare", "unstamped", "repo"} {
		key := filepath.Join(root, name)
		if f.CreatedAt[key] == 0 || f.CreatedAt[key] != createdAtOf(t, m, name) {
			t.Errorf("index[%s] = %d, want the in-memory CreatedAt %d", name, f.CreatedAt[key], createdAtOf(t, m, name))
		}
	}
	if got := f.CreatedAt[filepath.Join(root, "stamped")]; got != 100 {
		t.Errorf("index[stamped] = %d, want yaml's 100", got)
	}
}

// A non-zero created_at in project.yaml beats the index entry, and the index
// follows it.
func TestScan_YAMLCreatedAtBeatsIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "p", &ProjectConfig{CreatedAt: 900})
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	writeIndexFile(t, indexPath, map[string]int64{filepath.Join(root, "p"): 5})

	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, m, "p"); got != 900 {
		t.Errorf("CreatedAt = %d, want yaml's 900", got)
	}
	if got := readIndexFile(t, indexPath).CreatedAt[filepath.Join(root, "p")]; got != 900 {
		t.Errorf("index entry = %d, want 900", got)
	}
}

// The index is keyed by absolute path: a second root with the same basenames
// neither inherits nor erases the first root's entries.
func TestScan_IndexSurvivesRootChange(t *testing.T) {
	t.Parallel()
	rootA, rootB := t.TempDir(), t.TempDir()
	makeProjectDir(t, rootA, "proj", nil)
	makeProjectDir(t, rootB, "proj", nil)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	keyA := filepath.Join(rootA, "proj")
	writeIndexFile(t, indexPath, map[string]int64{keyA: 42})

	mB, _ := NewManager(rootB, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := mB.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, mB, "proj"); got == 42 || got == 0 {
		t.Errorf("rootB/proj CreatedAt = %d, want a fresh stamp (not rootA's 42)", got)
	}
	if got := readIndexFile(t, indexPath).CreatedAt[keyA]; got != 42 {
		t.Errorf("rootA entry after scanning rootB = %d, want 42 kept", got)
	}

	mA, _ := NewManager(rootA, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := mA.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, mA, "proj"); got != 42 {
		t.Errorf("rootA/proj CreatedAt after switching back = %d, want 42", got)
	}
}

// Entries are dropped once their directory leaves the root, but a directory
// skipped for a bad project.yaml keeps its place.
func TestScan_IndexPrunesRemovedDirsOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "gone", nil)
	makeProjectDir(t, root, "broken", nil)
	if err := os.MkdirAll(filepath.Join(root, "broken", ".naozhi"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken", ".naozhi", "project.yaml"), []byte("{{{"), 0600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	keyGone, keyBroken := filepath.Join(root, "gone"), filepath.Join(root, "broken")
	writeIndexFile(t, indexPath, map[string]int64{keyGone: 7, keyBroken: 8})
	if err := os.Remove(keyGone); err != nil {
		t.Fatal(err)
	}

	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	got := readIndexFile(t, indexPath).CreatedAt
	if _, ok := got[keyGone]; ok {
		t.Errorf("index kept removed dir: %v", got)
	}
	if got[keyBroken] != 8 {
		t.Errorf("index[broken] = %d, want 8 kept while its yaml is unparseable", got[keyBroken])
	}
}

// A corrupt index cannot be rebuilt, so it is moved aside rather than
// overwritten, and the scan still succeeds.
func TestScan_CorruptIndexPreserved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "p", nil)
	stateDir := t.TempDir()
	indexPath := filepath.Join(stateDir, "projects-index.json")
	if err := os.WriteFile(indexPath, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan with corrupt index: %v", err)
	}
	if createdAtOf(t, m, "p") == 0 {
		t.Error("CreatedAt not stamped")
	}
	matches, _ := filepath.Glob(indexPath + ".corrupt.*")
	if len(matches) != 1 {
		t.Fatalf("corrupt siblings = %v, want exactly one", matches)
	}
	if data, _ := os.ReadFile(matches[0]); string(data) != "{not json" {
		t.Errorf("preserved bytes = %q", data)
	}
	if readIndexFile(t, indexPath).CreatedAt[filepath.Join(root, "p")] == 0 {
		t.Error("fresh index not written after moving the corrupt one aside")
	}
}

// An index this build cannot use but that is still in place (over the cap,
// or written by a newer version) is never overwritten.
func TestScan_UnusableIndexNotOverwritten(t *testing.T) {
	t.Parallel()
	newer, err := json.Marshal(map[string]any{"version": projectsIndexVersion + 1, "created_at": map[string]int64{}})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"oversized": append([]byte(`{"version":1,"created_at":{}}`), bytes.Repeat([]byte(" "), projectsIndexMaxBytes)...),
		"newer":     newer,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			makeProjectDir(t, root, "p", nil)
			indexPath := filepath.Join(t.TempDir(), "projects-index.json")
			if err := os.WriteFile(indexPath, content, 0600); err != nil {
				t.Fatal(err)
			}
			m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
			if err := m.Scan(); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if createdAtOf(t, m, "p") == 0 {
				t.Error("CreatedAt not stamped in memory")
			}
			if got, _ := os.ReadFile(indexPath); !bytes.Equal(got, content) {
				t.Errorf("index was rewritten (%d bytes, was %d)", len(got), len(content))
			}
		})
	}
}

// An unchanged order does not rewrite the index on every 60s scan.
func TestScan_IndexWrittenOnlyOnChange(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "p", nil)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	// Same content, different bytes: a rewrite would normalise the formatting.
	marked, err := json.MarshalIndent(readIndexFile(t, indexPath), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, marked, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(indexPath); !bytes.Equal(got, marked) {
		t.Errorf("unchanged scan rewrote the index: %s", got)
	}

	makeProjectDir(t, root, "q", nil)
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if readIndexFile(t, indexPath).CreatedAt[filepath.Join(root, "q")] == 0 {
		t.Error("new project not written to the index")
	}
}

// Without an index path the order is still stable across scans of one
// Manager, and a directory added later is not stamped ahead of existing ones.
func TestScan_InMemoryIndexStableOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "zeta", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	first := createdAtOf(t, m, "zeta")
	makeProjectDir(t, root, "alpha", nil)
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, m, "zeta"); got != first {
		t.Errorf("zeta CreatedAt moved %d -> %d across scans", first, got)
	}
	if got := createdAtOf(t, m, "alpha"); got < first {
		t.Errorf("alpha CreatedAt = %d < zeta's %d; a later dir must not sort first", got, first)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(root, e.Name(), ".naozhi")); !os.IsNotExist(err) {
			t.Errorf("Scan created %s/.naozhi", e.Name())
		}
	}
}

// The synthetic include_root project is ordered in memory and never indexed.
func TestScan_IncludeRoot_NotIndexed(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "workspace")
	makeProjectDir(t, root, "alpha", nil)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	m, _ := NewManager(root, PlannerDefaults{}, WithIncludeRoot(true), WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readIndexFile(t, indexPath).CreatedAt[root]; ok {
		t.Error("root project was written to the index")
	}
}

// A config PUT that omits created_at keeps the project's place instead of
// zeroing it (which made the next Scan re-stamp it to the bottom).
func TestUpdateConfig_OmittedCreatedAtKeepsOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "a", &ProjectConfig{CreatedAt: 100})
	makeProjectDir(t, root, "b", &ProjectConfig{CreatedAt: 200})
	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateConfig("a", ProjectConfig{PlannerModel: "opus"}); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, m, "a"); got != 100 {
		t.Errorf("in-memory CreatedAt = %d, want 100 kept", got)
	}
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, m, "a"); got != 100 {
		t.Errorf("CreatedAt after rescan = %d, want 100 kept", got)
	}
	if all := m.All(); all[0].Name != "a" {
		t.Errorf("order = %v, want a first", projectNames(all))
	}

	if err := m.UpdateConfig("a", ProjectConfig{CreatedAt: 300}); err != nil {
		t.Fatal(err)
	}
	if got := createdAtOf(t, m, "a"); got != 300 {
		t.Errorf("explicit CreatedAt = %d, want 300", got)
	}
}

// An explicit write on a project with no yaml creates project.yaml carrying
// the index CreatedAt, so the order does not change once yaml takes over.
func TestBindChat_NoYAML_WritesIndexCreatedAt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "p", nil)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")
	writeIndexFile(t, indexPath, map[string]int64{filepath.Join(root, "p"): 4242})
	m, _ := NewManager(root, PlannerDefaults{}, WithIndexPath(indexPath))
	if err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := m.BindChat("p", "feishu", "direct", "u1"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(filepath.Join(root, "p"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CreatedAt != 4242 || len(cfg.ChatBindings) != 1 {
		t.Errorf("project.yaml = %+v, want created_at 4242 and one binding", cfg)
	}
}

func projectNames(ps []*Project) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return strings.Join(names, ",")
}
