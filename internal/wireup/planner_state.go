package wireup

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/project"
)

// PlannerState is the Planner's on-disk state under the state root
// (docs/rfc/planner.md §3.7). A nil field means that part could not be opened.
type PlannerState struct {
	Journal    *journal.Journal
	Workspaces *workspace.Registry
}

// OpenPlannerState creates the Planner state root next to the session store,
// opens the journal (pruning month files past retention) and loads the
// workspace registry for nodeID, registering the scanned projects on first
// start. Failures are logged, never fatal: no existing behaviour depends on
// this state. A zero layout (no store path) opens nothing.
func OpenPlannerState(lay datadir.Layout, nodeID string, projects *project.Manager) PlannerState {
	var st PlannerState
	root := lay.PlannerRoot()
	if root == "" {
		return st
	}
	if err := datadir.EnsureDir(root); err != nil {
		slog.Warn("planner state root unavailable", "path", root, "err", err)
		return st
	}
	if j, err := journal.Open(lay.PlannerLogRoot()); err != nil {
		slog.Warn("planner journal unavailable", "err", err)
	} else {
		if n, err := j.Prune(time.Now()); err != nil {
			slog.Warn("planner journal prune failed", "err", err)
		} else if n > 0 {
			slog.Info("planner journal pruned", "files", n)
		}
		st.Journal = j
	}
	if projects == nil {
		st.Workspaces = workspace.Load(lay.PlannerWorkspacesPath(), nodeID)
	} else {
		// The projects root holds workspaces rather than being one, so the
		// include_root synthetic project is skipped as a seed too.
		st.Workspaces = workspace.Load(lay.PlannerWorkspacesPath(), nodeID, projects.Root())
		var seeds []workspace.Seed
		for _, p := range projects.All() {
			seeds = append(seeds, workspace.Seed{Name: p.Name, Path: p.Path})
		}
		if n := st.Workspaces.SeedIfEmpty(seeds); n > 0 {
			slog.Info("planner workspaces registered from projects", "count", n)
		}
	}
	return st
}
