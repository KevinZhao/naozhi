package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

const node = "mac"

var idRE = regexp.MustCompile(`^ws_[a-z0-9]{8}$`)

// tempRoot returns a symlink-resolved temp dir holding the given subdirectories.
func tempRoot(t *testing.T, dirs ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSeedLookupEnsure(t *testing.T) {
	root := tempRoot(t, "naozhi/internal", "nested/inner", "other")
	path := filepath.Join(root, "state", "workspaces.json")
	r := Load(path, node)

	n := r.SeedIfEmpty([]Seed{
		{Name: "naozhi", Path: filepath.Join(root, "naozhi")},
		{Name: "dup", Path: filepath.Join(root, "naozhi") + "/"},
		{Name: "nested", Path: filepath.Join(root, "nested")},
		{Name: "inner", Path: filepath.Join(root, "nested", "inner")},
		{Name: "relative", Path: "relative/path"},
	})
	if n != 3 {
		t.Fatalf("SeedIfEmpty added %d, want 3", n)
	}
	if again := r.SeedIfEmpty([]Seed{{Name: "x", Path: filepath.Join(root, "other")}}); again != 0 {
		t.Errorf("second SeedIfEmpty added %d, want 0", again)
	}

	cases := []struct {
		dir  string
		want string // workspace name, "" for no match
	}{
		{filepath.Join(root, "naozhi"), "naozhi"},
		{filepath.Join(root, "naozhi", "internal"), "naozhi"},
		{filepath.Join(root, "naozhi", "gone", "later"), "naozhi"},
		{filepath.Join(root, "nested", "inner", "x"), "inner"},
		{filepath.Join(root, "nested"), "nested"},
		{filepath.Join(root, "naozhi-fork"), ""},
		{filepath.Join(root, "other"), ""},
		{"relative", ""},
	}
	for _, c := range cases {
		w, ok := r.Lookup(c.dir)
		if got := map[bool]string{true: w.Name}[ok]; got != c.want {
			t.Errorf("Lookup(%s) = %q, want %q", c.dir, got, c.want)
		}
	}

	other := filepath.Join(root, "other")
	w, ok := r.Ensure(other)
	if !ok {
		t.Fatal("Ensure(other) refused")
	}
	if w.Name != "other" || !idRE.MatchString(w.ID) || len(w.Paths[node]) != 1 || w.Paths[node][0] != other {
		t.Errorf("Ensure registered %+v", w)
	}
	again, ok := r.Ensure(filepath.Join(other, "sub"))
	if !ok || again.ID != w.ID {
		t.Errorf("Ensure(child) = %+v, %v; want existing %s", again, ok, w.ID)
	}
	if _, ok := r.Ensure("relative"); ok {
		t.Error("Ensure accepted a relative path")
	}

	// Reloading keeps every ID; the file is the documented v1 shape.
	before := r.List()
	after := Load(path, node).List()
	if len(after) != len(before) {
		t.Fatalf("reloaded %d workspaces, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Name != before[i].Name || !after[i].Created.Equal(before[i].Created) {
			t.Errorf("reloaded[%d] = %+v, want %+v", i, after[i], before[i])
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		V          int `json:"v"`
		Workspaces []struct {
			ID      string              `json:"id"`
			Paths   map[string][]string `json:"paths"`
			Created string              `json:"created"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.V != 1 || len(f.Workspaces) != 4 {
		t.Fatalf("file = %s (err %v)", raw, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("registry mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestLookupCaseFolded: ~/Workspace and ~/workspace are one directory on a
// case-insensitive filesystem, and a session cwd spelled either way must land
// in the same workspace.
func TestLookupCaseFolded(t *testing.T) {
	root := tempRoot(t, "Workspace/naozhi")
	folded := filepath.Join(root, "workspace", "naozhi")
	if _, err := os.Stat(folded); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	r := Load("", node)
	r.SeedIfEmpty([]Seed{{Name: "naozhi", Path: filepath.Join(root, "Workspace", "naozhi")}})
	if w, ok := r.Lookup(filepath.Join(folded, "internal")); !ok || w.Name != "naozhi" {
		t.Errorf("Lookup(case-folded) = %+v, %v", w, ok)
	}
	// A shallower path matching byte-wise must not hide the deeper folded one.
	if _, ok := r.Ensure(filepath.Join(root, "workspace")); !ok {
		t.Fatal("Ensure(workspace) refused")
	}
	if w, ok := r.Lookup(filepath.Join(folded, "internal")); !ok || w.Name != "naozhi" {
		t.Errorf("Lookup(case-folded, shallow byte match) = %+v, %v", w, ok)
	}
}

// TestEnsureContainer: the projects root and its ancestors hold workspaces
// rather than being one, so registering one would claim every project
// created below it later.
func TestEnsureContainer(t *testing.T) {
	root := tempRoot(t, "projects/proj", "projects/newproj")
	projects := filepath.Join(root, "projects")
	r := Load("", node, projects)
	if n := r.SeedIfEmpty([]Seed{{Path: projects}, {Path: filepath.Join(projects, "proj")}}); n != 1 {
		t.Fatalf("SeedIfEmpty added %d, want 1 (container skipped)", n)
	}
	for _, dir := range []string{projects, root} {
		if w, ok := r.Ensure(dir); ok {
			t.Errorf("Ensure(%s) registered %+v", dir, w)
		}
	}
	w, ok := r.Ensure(filepath.Join(projects, "newproj"))
	if !ok || w.Name != "newproj" || len(r.List()) != 2 {
		t.Errorf("Ensure(newproj) = %+v, %v; want a new workspace", w, ok)
	}
}

// TestLoadCorruptFailsClosed: IDs cannot be regenerated, so a registry that no
// longer parses is left in place and never replaced by a fresh seed, on this
// start or the next.
func TestLoadCorruptFailsClosed(t *testing.T) {
	root := tempRoot(t, "repo")
	path := filepath.Join(root, "workspaces.json")
	orig := []byte(`{"v":1,"workspaces":[{"id":"ws_aaaaaaaa","name":"renamed",,"paths":{}}]}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r := Load(path, node)
		r.SeedIfEmpty([]Seed{{Path: filepath.Join(root, "repo")}})
		r.Ensure(filepath.Join(root, "repo"))
		if got, _ := os.ReadFile(path); string(got) != string(orig) {
			t.Fatalf("corrupt registry overwritten: %s", got)
		}
	}
}

// TestLookupOtherNode: paths registered for another node never match here.
func TestLookupOtherNode(t *testing.T) {
	root := tempRoot(t, "repo")
	repo := filepath.Join(root, "repo")
	path := filepath.Join(root, "workspaces.json")
	data, _ := json.Marshal(file{V: Version, Workspaces: []Workspace{
		{ID: "ws_remote01", Name: "repo", Paths: map[string][]string{"other-node": {repo}}},
	}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Load(path, node)
	if _, ok := r.Lookup(repo); ok {
		t.Error("matched a path registered for another node")
	}
	w, ok := r.Ensure(repo)
	if !ok || w.ID == "ws_remote01" || len(r.List()) != 2 {
		t.Errorf("Ensure = %+v, %v; want a new local workspace", w, ok)
	}
}

// TestLookupOn: a remote node's paths are matched per node and lexically,
// never through this machine's file system.
func TestLookupOn(t *testing.T) {
	root := tempRoot(t, "real")
	real, link := filepath.Join(root, "real"), filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspaces.json")
	data, _ := json.Marshal(file{V: Version, Workspaces: []Workspace{
		{ID: "ws_app00001", Name: "app", Paths: map[string][]string{"mini": {"/srv/app"}}},
		{ID: "ws_app00002", Name: "app-box", Paths: map[string][]string{"box": {"/srv/app"}}},
		{ID: "ws_sub00001", Name: "sub", Paths: map[string][]string{"mini": {"/srv/app/sub"}}},
		{ID: "ws_data0001", Name: "data", Paths: map[string][]string{"mini": {"/data/x/"}}},
		{ID: "ws_real0001", Name: "real", Paths: map[string][]string{"mini": {real}}},
		{ID: "ws_local001", Name: "local", Paths: map[string][]string{node: {"/srv/app"}}},
	}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Load(path, node)
	cases := []struct {
		node, dir string
		want      string // workspace name, "" for no match
	}{
		{"mini", "/srv/app", "app"},
		{"box", "/srv/app/x", "app-box"},
		{"mini", "/srv/app/sub/deep", "sub"},
		{"mini", "/srv/app/../app/sub/", "sub"},
		{"mini", "/srv/application", ""},
		{"mini", "/data/x/y", "data"},
		{"mini", link, ""}, // the link exists here, not on mini
		{"other", "/srv/app", ""},
		{"", "/srv/app", ""},
		{"mini", "srv/app", ""},
	}
	for _, c := range cases {
		w, ok := r.LookupOn(c.node, c.dir)
		if got := map[bool]string{true: w.Name}[ok]; got != c.want {
			t.Errorf("LookupOn(%s, %s) = %q, want %q", c.node, c.dir, got, c.want)
		}
	}
	if w, ok := r.Lookup("/srv/app"); !ok || w.Name != "local" {
		t.Errorf("Lookup(/srv/app) = %+v, %v; want this node's workspace", w, ok)
	}
}

func TestLoadNewerVersionNotOverwritten(t *testing.T) {
	root := tempRoot(t, "repo")
	path := filepath.Join(root, "workspaces.json")
	orig := []byte(`{"v":2,"workspaces":[]}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	r := Load(path, node)
	if _, ok := r.Ensure(filepath.Join(root, "repo")); !ok {
		t.Fatal("Ensure refused")
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Errorf("newer-version registry overwritten: %s", got)
	}
}
