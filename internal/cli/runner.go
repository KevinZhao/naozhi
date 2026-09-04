package cli

import "context"

// Placement identifies WHERE a CLI job runs — the placement axis of the
// agentcore-cloud-sandbox RFC (§4.2): a job is flavor (backend.Profile) ×
// placement (Runner). "local" is the only placement today.
type Placement string

// PlacementLocal runs the CLI as a child process on this host via the shim.
const PlacementLocal Placement = "local"

// Runner abstracts where a CLI process is spawned (placement axis), so the
// session router depends on "something that can start a job" rather than the
// local exec + shim transport (RFC §4.2). Reconnect is deliberately NOT part of
// it: shim reattach is local-only (sandbox jobs are run-once, RFC §3.1), so
// SpawnReconnect stays a *Wrapper method.
type Runner interface {
	// Spawn starts a CLI job at this placement; semantics match (*Wrapper).Spawn.
	Spawn(ctx context.Context, opts SpawnOptions) (*Process, error)
	// Placement reports where this runner executes jobs — for dispatch, run
	// records and dashboard badges (RFC §7.2), never for branching inside cli.
	Placement() Placement
}

// localRunner adapts *Wrapper to Runner by pure delegation.
type localRunner struct {
	w *Wrapper
}

var _ Runner = (*localRunner)(nil)

func (r *localRunner) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error) {
	return r.w.Spawn(ctx, opts)
}

func (r *localRunner) Placement() Placement {
	return PlacementLocal
}

// Runner returns the wrapper's placement runner (today always local); nil-safe
// like Manager(). Treat it as "the local runner", not a registry: placement
// selection belongs at spawn-params resolution (RFC §4.2). Orthogonal to the
// cli.Transport seam (#721): Transport = how bytes move, Runner = WHERE it runs.
func (w *Wrapper) Runner() Runner {
	if w == nil {
		return nil
	}
	return &localRunner{w: w}
}
