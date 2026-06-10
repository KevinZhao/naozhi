package cli

import "context"

// Placement identifies WHERE a CLI job runs — the placement axis of the
// agentcore-cloud-sandbox RFC (§4.2): a job is flavor × placement, where
// flavor (claude / kiro) is owned by backend.Profile and placement is owned
// by the Runner seam below. "local" is the only placement today; "sandbox"
// (AgentCore microVM) arrives with the agentcoreRunner.
type Placement string

// PlacementLocal runs the CLI as a child process on this host via the shim
// transport — the historical (and default) behaviour.
const PlacementLocal Placement = "local"

// Runner abstracts where a CLI process is spawned (placement axis), so the
// session router depends on "something that can start a job" instead of the
// concrete local exec + shim transport. agentcore-cloud-sandbox RFC §4.2:
// localRunner is the pure extraction of today's behaviour; agentcoreRunner
// (InvokeAgentRuntime + payload injection + hold event-stream) implements
// the same interface in a later PR without touching protocol parsing —
// both placements speak claude stream-json through the same Protocol.
//
// Deliberately small (Go style: 1-3 methods). Reconnect is NOT part of the
// interface: shim reattach is a local-placement capability (sandbox jobs
// are run-once and never reattach, RFC §3.1), so SpawnReconnect stays a
// *Wrapper method and local-only callers keep using it directly.
type Runner interface {
	// Spawn starts a new CLI job at this placement and returns a connected
	// Process. Semantics match (*Wrapper).Spawn for the local placement.
	Spawn(ctx context.Context, opts SpawnOptions) (*Process, error)
	// Placement reports where this runner executes jobs. Used for
	// dispatch decisions, run-record metadata, and dashboard badges
	// (RFC §7.2) — never for behavioural branching inside cli.
	Placement() Placement
}

// localRunner adapts *Wrapper to the Runner interface. Pure delegation —
// zero behaviour change. Constructed via (*Wrapper).Runner().
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

// Runner returns the wrapper's placement runner (today always local).
// Nil-safe: a nil wrapper yields a nil Runner so callers can keep their
// existing `wrapper == nil` guard semantics — mirrors Manager()'s
// nil-receiver contract.
func (w *Wrapper) Runner() Runner {
	if w == nil {
		return nil
	}
	return &localRunner{w: w}
}
