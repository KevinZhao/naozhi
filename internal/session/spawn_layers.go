package session

import "github.com/naozhi/naozhi/internal/shim"

// argvLayers is the merged output of mergeArgvLayers: the values that
// argvSpawnOptions turns into argv (model, effort, extra args, and the
// appended system prompt).
type argvLayers struct {
	Model  string
	Effort string
	Args   []string
	// SystemPrompt is the overlay's AppendSystemPrompt passed through unchanged
	// (#2493): the agent → planner → scratch layering already happened in the
	// resolvers that built AgentOpts.SystemPrompt.
	SystemPrompt string
}

// mergeArgvLayers is the single, side-effect-free precedence rule for the
// argv-bearing spawn parameters, shared by resolveSpawnParamsLocked (real spawn)
// and driftCompareArgs (drift on reconnect) so the two argv only differ when
// something genuinely changed since the shim was spawned (#2494). Pure: no
// Router access; a fresh Args slice is returned so no caller aliases bd.Args.
//
//	model:  bd.Model ← profileDefaultModel ← ov.Model ← tuningModel  (low → high)
//	effort: bd.Effort ← ov.Effort ← tuningEffort  (no profile tier: docs/rfc/kiro-effort-control.md §4.2)
//	args:   bd.Args ++ ov.ExtraArgs  (append, never replace)
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

	// Session tuning is the TOP of both chains: a dashboard pick for THIS
	// session outranks every config tier (docs/rfc/dashboard-model-effort-control.md §4.3).
	if tuningModel != "" {
		model = tuningModel
	}
	if tuningEffort != "" {
		effort = tuningEffort
	}
	return argvLayers{Model: model, Effort: effort, Args: args, SystemPrompt: ov.AppendSystemPrompt}
}

// profileDefaultModelFor returns the default_model of profile id in profiles,
// or "" when id is empty or unknown. Pure lookup shared by the spawn path
// (r.accessProfiles under r.mu write lock) and the drift path
// (accessProfileDefaultModel) so the two cannot disagree.
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
// copy-on-write (AddAccessProfile swaps the whole map under the write lock).
func (r *Router) accessProfileDefaultModel(id string) string {
	if id == "" {
		return ""
	}
	r.mu.RLock()
	profiles := r.accessProfiles
	r.mu.RUnlock()
	// Safe after RUnlock: writers never mutate this map in place.
	return profileDefaultModelFor(profiles, id)
}
