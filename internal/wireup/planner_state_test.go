package wireup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/project"
)

func TestOpenPlannerState(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{}, project.WithIncludeRoot(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatal(err)
	}
	lay := datadir.ForStore(filepath.Join(t.TempDir(), "sessions.json"))

	st := OpenPlannerState(lay, "mac", mgr)
	if st.Journal == nil || st.Workspaces == nil {
		t.Fatalf("state = %+v", st)
	}
	defer st.Journal.Close()
	if err := st.Journal.Append(journal.Entry{Kind: journal.KindHeartbeat, Unit: "test"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range st.Workspaces.List() {
		names = append(names, w.Name)
	}
	if !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Errorf("seeded workspaces = %v, want [alpha beta] (root project skipped)", names)
	}
	if fi, err := os.Stat(lay.PlannerRoot()); err != nil || fi.Mode().Perm() != datadir.DirMode {
		t.Errorf("planner root = %v, %v", fi, err)
	}

	// A second start keeps the registered IDs rather than seeding again.
	again := OpenPlannerState(lay, "mac", mgr)
	if got, want := again.Workspaces.List(), st.Workspaces.List(); len(got) != len(want) || got[0].ID != want[0].ID {
		t.Errorf("restart registry = %+v, want %+v", got, want)
	}
	if again.Journal != nil {
		again.Journal.Close()
	}

	if empty := OpenPlannerState(datadir.Layout{}, "mac", nil); empty.Journal != nil || empty.Workspaces != nil {
		t.Errorf("zero layout opened %+v", empty)
	}
}
