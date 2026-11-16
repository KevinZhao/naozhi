package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// TestReadRulesSymlinkedWorkspace: a workspace a session reached through a
// symlink (/tmp → /private/tmp on macOS) is readable by both spellings, and
// a tainted event log and the data root's secrets inside it are denied by
// every spelling the allow rules open, the state root's own one included.
// The rules come out the same on every build, so a refresh restarts nothing.
func TestReadRulesSymlinkedWorkspace(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "cwd"), 0o700); err != nil {
		t.Fatal(err)
	}
	link, link2 := filepath.Join(base, "link"), filepath.Join(base, "link2")
	for _, l := range []string{link, link2} {
		if err := os.Symlink(real, l); err != nil {
			t.Fatal(err)
		}
	}
	reg := workspace.Load("", "local")
	if _, ok := reg.Ensure(link); !ok {
		t.Fatal("workspace not registered")
	}
	if _, ok := reg.Ensure(filepath.Join(link2, "cwd")); !ok {
		t.Fatal("subdirectory not found in the workspace")
	}

	now := time.Now()
	key := "dashboard:direct:u:general"
	a := New(Config{
		Enabled:    true,
		Layout:     datadir.FromRoot(filepath.Join(link, "state")),
		Home:       t.TempDir(),
		Sessions:   &fakeSessions{},
		Now:        func() time.Time { return now },
		Workspaces: reg,
		Tainted:    func() []string { return []string{key} },
	})
	allow, deny := a.readRules()
	for _, dir := range []string{real, link, link2} {
		if r := absRule("Read", filepath.Join(dir, "**")); !slices.Contains(allow, r) {
			t.Errorf("allow lacks %s: %q", r, allow)
		}
		for _, r := range []string{
			absRule("Read", filepath.Join(dir, "state", "events", persist.KeyHash(key)+"*")),
			absRule("Read", filepath.Join(dir, "state", "*.yaml")),
		} {
			if !slices.Contains(deny, r) {
				t.Errorf("deny lacks %s: %q", r, deny)
			}
		}
	}
	for range 5 {
		if a2, d2 := a.readRules(); !slices.Equal(a2, allow) || !slices.Equal(d2, deny) {
			t.Fatalf("rules differ between builds:\n%q\n%q", a2, d2)
		}
	}
}

// TestSpellingsNotYetCreated: a denied directory that does not exist yet is
// spelled through its existing ancestor, so creating it changes no rule.
func TestSpellingsNotYetCreated(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(link, "state", "events")
	before := spellings(dir, nil)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if after := spellings(dir, nil); !slices.Equal(before, after) {
		t.Fatalf("spellings changed once created: %q → %q", before, after)
	}
	if want := []string{dir, filepath.Join(base, "state", "events")}; !slices.Equal(before, want) {
		t.Fatalf("spellings = %q, want %q", before, want)
	}
}

// TestReadRulesSymlinkedHome: home reached by a second spelling, through a
// workspace alias or as the CLI was given it, keeps its credentials denied
// by every spelling; the CLI expands "~" to only one.
func TestReadRulesSymlinkedHome(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{real, link} {
		reg := workspace.Load("", "local")
		if _, ok := reg.Ensure(link); !ok {
			t.Fatal("home not registered")
		}
		a := New(Config{
			Enabled:    true,
			Layout:     datadir.FromRoot(filepath.Join(base, "state")),
			Home:       home,
			Sessions:   &fakeSessions{},
			Workspaces: reg,
		})
		allow, deny := a.readRules()
		other := real
		if home == real {
			other = link
		}
		if r := absRule("Read", filepath.Join(other, "**")); home == real && !slices.Contains(allow, r) {
			t.Errorf("HOME=%s: allow lacks %s: %q", home, r, allow)
		}
		for _, p := range []string{".aws/**", ".claude/settings*.json", ".netrc"} {
			if r := absRule("Read", filepath.Join(other, p)); !slices.Contains(deny, r) {
				t.Errorf("HOME=%s: deny lacks %s: %q", home, r, deny)
			}
		}
	}
}

// TestReadRulesAliasInsideDeniedTree: a workspace inside the data root or
// a denied directory, registered through a symlink, keeps what is denied
// there denied by its alias's spelling too.
func TestReadRulesAliasInsideDeniedTree(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	for _, d := range []string{"state/sys/x", "state/shims/y", "shimstate/z"} {
		if err := os.MkdirAll(filepath.Join(real, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	reg := workspace.Load("", "local")
	for _, d := range []string{"state/sys/x", "state/shims/y", "shimstate/z"} {
		if _, ok := reg.Ensure(filepath.Join(link, d)); !ok {
			t.Fatalf("%s not registered", d)
		}
	}
	a := New(Config{
		Enabled:    true,
		Layout:     datadir.FromRoot(filepath.Join(real, "state")),
		Home:       t.TempDir(),
		Sessions:   &fakeSessions{},
		Workspaces: reg,
		DenyPaths:  []string{filepath.Join(real, "shimstate")},
	})
	allow, deny := a.readRules()
	if r := absRule("Read", filepath.Join(link, "state", "sys", "x", "**")); !slices.Contains(allow, r) {
		t.Fatalf("allow lacks %s: %q", r, allow)
	}
	for _, p := range []string{"state/sys/x/**/*.yaml", "state/shims/y/**", "shimstate/z/**"} {
		if r := absRule("Read", filepath.Join(link, p)); !slices.Contains(deny, r) {
			t.Errorf("deny lacks %s: %q", r, deny)
		}
	}
	if r := absRule("Read", filepath.Join(link, "state", "sys", "x", "*.json")); slices.Contains(deny, r) {
		t.Errorf("the data root's top-level *.json reached into a subdirectory: %s", r)
	}
}

func TestPatternBelow(t *testing.T) {
	for _, c := range []struct {
		pat, rel, want string
		ok             bool
	}{
		{"**/*.yaml", "a/b", "**/*.yaml", true},
		{"*.yaml", "a", "", false},
		{"shims/**", "shims", "**", true},
		{"shims/**", "shims/x", "**", true},
		{"shims/**", "other", "", false},
		{"", "a", "**", true},
		{".aws/**", ".aws", "**", true},
		{".claude/settings*.json", ".claude", "settings*.json", true},
		{"abc*", "abcdef/g", "**", true},
		{"abc*", "xyz", "", false},
	} {
		got, ok := patternBelow(filepath.FromSlash(c.pat), filepath.FromSlash(c.rel))
		if got != filepath.FromSlash(c.want) || ok != c.ok {
			t.Errorf("patternBelow(%q, %q) = %q, %v; want %q, %v", c.pat, c.rel, got, ok, c.want, c.ok)
		}
	}
}
