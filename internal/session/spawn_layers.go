package session

import "github.com/naozhi/naozhi/internal/shim"

// argvLayers is the merged output of mergeArgvLayers: the three values that
// argvSpawnOptions turns into argv (model, effort, extra args).
type argvLayers struct {
	Model  string
	Effort string
	Args   []string
}

// mergeArgvLayers is the single, side-effect-free precedence rule for the
// argv-bearing spawn parameters. Both consumers call it (#2494):
//
//   - resolveSpawnParamsLocked, for the REAL spawn — bd from current config,
//     ov from the caller's AgentOpts + resolved access profile, tuning from
//     the existing session entry; and
//   - driftCompareArgs, for the arg-drift comparison on reconnect — bd from
//     current config, ov from the surviving shim's persisted state, tuning
//     from the live session entry.
//
// Same function, same inputs shape, so the two argv can only differ when
// something that genuinely changed since the shim was spawned (a config edit,
// a dashboard tuning pick) changed them. Before this helper the drift side
// re-implemented the merge WITHOUT the overlay tier and every agents[].model /
// agents[].effort session was killed on every naozhi restart.
//
// Precedence (lowest → highest), unchanged from the inline code it replaces:
//
//	model:  bd.Model ← profileDefaultModel ← ov.Model ← tuningModel
//	effort: bd.Effort ← ov.Effort ← tuningEffort   (no profile tier, by design:
//	        docs/rfc/kiro-effort-control.md §4.2)
//	args:   bd.Args ++ ov.ExtraArgs                (append, never replace)
//
// profileDefaultModel is taken already-resolved (see profileDefaultModelFor)
// so this stays a pure function and the caller owns the lock discipline of
// the r.accessProfiles read.
//
// Pure: no Router access, no I/O, no logging; returns a fresh Args slice so
// neither caller can alias the backend's configured slice.
func mergeArgvLayers(bd backendDefaults, profileDefaultModel string, ov shim.SpawnOverlay, tuningModel, tuningEffort string) argvLayers {
	model := bd.Model
	if profileDefaultModel != "" {
		model = profileDefaultModel
	}
	if ov.Model != "" {
		model = ov.Model
	}
	args := make([]string, 0, len(bd.Args)+len(ov.ExtraArgs))
	args = append(args, bd.Args...)
	args = append(args, ov.ExtraArgs...)

	effort := bd.Effort
	if ov.Effort != "" {
		effort = ov.Effort
	}

	// Session tuning is the TOP of both chains: an operator's dashboard pick
	// for THIS session outranks every config tier and the per-request opts.
	// docs/rfc/dashboard-model-effort-control.md §4.3.
	if tuningModel != "" {
		model = tuningModel
	}
	if tuningEffort != "" {
		effort = tuningEffort
	}
	return argvLayers{Model: model, Effort: effort, Args: args}
}

// profileDefaultModelFor returns the default_model of profile id in profiles,
// or "" when id is empty or unknown. Pure lookup shared by the spawn path
// (which reads r.accessProfiles under r.mu write lock) and the drift path
// (which snapshots the map under RLock via accessProfileDefaultModel) so the
// two cannot disagree on what "the profile's model" means.
func profileDefaultModelFor(profiles map[string]AccessProfile, id string) string {
	if id == "" {
		return ""
	}
	if ap, ok := profiles[id]; ok {
		return ap.DefaultModel
	}
	return ""
}

// accessProfileDefaultModel is the lock-taking form of profileDefaultModelFor
// for callers that do NOT hold r.mu (driftCompareArgs runs after
// reconnectShims releases it). RLock is sufficient: the registry is
// copy-on-write (AddAccessProfile swaps the whole map under the write lock),
// so the snapshot is a consistent map even if a dashboard "create profile"
// races the restart.
func (r *Router) accessProfileDefaultModel(id string) string {
	if id == "" {
		return ""
	}
	r.mu.RLock()
	profiles := r.accessProfiles
	r.mu.RUnlock()
	// Safe to read after RUnlock: writers never mutate this map in place, they
	// replace r.accessProfiles with a new one, so our snapshot stays immutable.
	return profileDefaultModelFor(profiles, id)
}
