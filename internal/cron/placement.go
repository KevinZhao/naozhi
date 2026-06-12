package cron

import "fmt"

// Placement values for Job.Placement — the WHERE axis of the
// agentcore-cloud-sandbox RFC (§4.2), orthogonal to Backend (the engine
// axis). cron keeps its own string constants instead of importing
// internal/cli: the run-once sandbox path is fire-and-forget (§3.1) and
// never constructs a cli.Process, so coupling cron to the cli Runner seam
// would buy nothing.
const (
	// PlacementLocal runs the job on this host through the session router
	// (the historical behaviour). The empty string means the same thing —
	// jobs created before the field existed keep working unchanged.
	PlacementLocal = "local"
	// PlacementSandbox runs the job as a run-once AgentCore microVM job:
	// payload injection, held event stream, burn on completion.
	PlacementSandbox = "sandbox"
)

// placementIsSandbox folds the ""≡local default.
func placementIsSandbox(p string) bool { return p == PlacementSandbox }

// validatePlacement gates the field at every write path (AddJob /
// UpdateJob / store load). Unknown values are rejected rather than
// defaulted: a typo silently falling back to local would run a job the
// operator explicitly wanted isolated.
func validatePlacement(p string) error {
	switch p {
	case "", PlacementLocal, PlacementSandbox:
		return nil
	default:
		return fmt.Errorf("invalid placement %q (want \"\", %q or %q)", p, PlacementLocal, PlacementSandbox)
	}
}
