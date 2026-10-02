package session

import (
	"log/slog"
	"slices"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// driftArgs rebuilds, for the startup arg-drift check, the argv a fresh spawn
// of a session would use today: backend defaults and access profiles from
// backends, the router-owned argv paths from spawn. reconnectShims builds one
// from Router's value fields; it reads no session-table state.
type driftArgs struct {
	backends *BackendRegistry
	spawn    *spawnConfig
}

// driftCompareArgs reconstructs the argv a fresh spawn of this session would
// use, for comparison against the shim-recorded argv (tuning_drift_parity_test.go).
// Backend defaults, the session's tuning overrides (--model/--effort — omitting
// them would flag every tuned session as drift on restart) and the overlay the
// shim persisted at spawn (agents[].model/.effort/.extra_args + access profile,
// #2494) are re-merged through the same mergeArgvLayers the spawn used. sess
// may be nil (adopt path). A nil overlay (pre-#2494 state) degrades to a
// backend-defaults-only comparison; the caller logs that once per shim.
// Reads no table state, so it runs outside any transaction.
func (d driftArgs) driftCompareArgs(recWrapper *cli.Wrapper, backendID, key string, sess *ManagedSession, overlay *shim.SpawnOverlay) []string {
	var ov shim.SpawnOverlay
	if overlay != nil {
		ov = *overlay
	}
	var tuningModel, tuningEffort string
	if sess != nil {
		tuningModel, tuningEffort = sess.TuningModel(), sess.TuningEffort()
	}
	merged := mergeArgvLayers(
		d.backends.backendDefaultsFor(backendID),
		d.backends.accessProfileDefaultModel(ov.AccessProfile),
		ov, tuningModel, tuningEffort)
	// Every argv-bearing field is mirrored by construction via argvSpawnOptions.
	// cliDebugPathFor (not cliDebugFileFor) keeps the comparison read-only.
	// systemPrompt is "" by design: AgentOpts.SystemPrompt is per-session and
	// not reconstructible here, so stripResumeArgs removes the stored
	// --append-system-prompt pair instead (#2493).
	opts := d.spawn.argvSpawnOptions(merged.Model, merged.Effort, d.spawn.cliDebugPathFor(key), merged.SystemPrompt, merged.Args)
	// Re-deriving argv re-hits the same gates every 30s reconcile tick; the
	// emitter's per-scope dedup keeps that as one Warn then Debug repeats.
	cli.EmitSpawnDiags(key, cli.SpawnDiagsFor(opts, cli.ProtocolCaps(recWrapper.Protocol)))
	return recWrapper.Protocol.BuildArgs(opts)
}

// shimArgsDrift is the arg-drift predicate for classifyShimState: does the
// argv the surviving shim recorded (minus the session-specific --resume pair)
// still equal what a fresh spawn would use today? Returns both argv so the
// caller can log the first divergence. Never drifts on an empty stored argv.
// A nil state.SpawnOverlay (shim spawned before the overlay was persisted,
// #2494) cannot see agents[].model/.effort and may restart that session ONCE;
// logged here so the operator can attribute the restart.
func (d driftArgs) shimArgsDrift(recWrapper *cli.Wrapper, backendID string, state shim.State, sess *ManagedSession) (drift bool, storedBase, currentArgs []string) {
	storedBase = stripResumeArgs(state.CLIArgs)
	if len(storedBase) == 0 {
		return false, storedBase, nil
	}
	if state.SpawnOverlay == nil {
		slog.Info("shim state predates spawn-overlay persistence; drift compare falls back to backend defaults",
			"key", state.Key, "pid", state.ShimPID)
	}
	currentArgs = d.driftCompareArgs(recWrapper, backendID, state.Key, sess, state.SpawnOverlay)
	return !slices.Equal(storedBase, currentArgs), storedBase, currentArgs
}

// stripResumeArgs removes --resume <id> pairs from a CLI arg slice for the
// drift check: --resume is session-specific, not a config change.
// `--append-system-prompt` is NOT stripped — it travels in the overlay and a
// changed prompt correctly reads as drift (#2493). Returns the original slice
// unchanged when --resume is absent.
func stripResumeArgs(args []string) []string {
	hasResume := false
	for _, a := range args {
		if a == "--resume" {
			hasResume = true
			break
		}
	}
	if !hasResume {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--resume" {
			// Skip the flag and its value; a trailing bare `--resume` must
			// also go or it spuriously reads as drift.
			if i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
