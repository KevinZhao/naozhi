package sysession

import "context"

// RunInfo identifies the daemon run a Runner call belongs to. Manager.runOnce
// stores it on the Tick context so the Runner can attribute the call's cost
// to the run without a signature change on every daemon.
type RunInfo struct {
	Daemon string
	RunID  string
}

type runInfoKey struct{}

func withRunInfo(ctx context.Context, daemon, runID string) context.Context {
	return context.WithValue(ctx, runInfoKey{}, RunInfo{Daemon: daemon, RunID: runID})
}

// RunInfoFromContext returns the run identity stored by Manager.runOnce;
// ok=false when the Runner was called outside a managed Tick.
func RunInfoFromContext(ctx context.Context) (RunInfo, bool) {
	ri, ok := ctx.Value(runInfoKey{}).(RunInfo)
	return ri, ok
}
