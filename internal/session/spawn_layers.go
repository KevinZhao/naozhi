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
// argv-bearing spawn parameters, shared by resolveSpawnParams (real spawn)
// and driftCompareArgs (drift on reconnect) so the two argv only differ when
// something genuinely changed since the shim was spawned (#2494). Pure: no
// Router access; a fresh Args slice is returned so no caller aliases bd.Args.
//
//	model:  bd.Model ← profileDefaultModel ← ov.Model ← tuningModel  (low → high)
//	effort: bd.Effort ← ov.Effort ← tuningEffort  (no profile tier: docs/rfc/kiro-effort-control.md §4.2)
//	args:   bd.Args ++ ov.ExtraArgs  (append, never replace)
func mergeArgvLayers(bd BackendDefaults, profileDefaultModel string, ov shim.SpawnOverlay, tuningModel, tuningEffort string) argvLayers {
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
// or "" when id is empty or unknown. Pure lookup shared by the spawn path and
// the drift path (accessProfileDefaultModel) so the two cannot disagree.
func profileDefaultModelFor(profiles map[string]AccessProfile, id string) string {
	if id == "" {
		return ""
	}
	if ap, ok := profiles[id]; ok {
		return ap.DefaultModel
	}
	return ""
}

// EffectiveDefaultBackend is the backend tier for a key with no session,
// below opts.Backend, the dashboard pick and resume continuity: the agent's
// backend (agents[].backend), else default_backend of access profile
// profileID, else "" (router default). Shared by the spawn path and `naozhi
// config check --effective` so the two cannot list an agent differently.
func EffectiveDefaultBackend(agentBackend string, profiles map[string]AccessProfile, profileID string) string {
	if agentBackend != "" {
		return agentBackend
	}
	if profileID == "" {
		return ""
	}
	return profiles[profileID].DefaultBackend
}

// accessProfileDefaultModel is profileDefaultModelFor over the current
// registry, for the drift check.
func (b *BackendRegistry) accessProfileDefaultModel(id string) string {
	return profileDefaultModelFor(b.profiles(), id)
}

// EffectiveArgvLayers is mergeArgvLayers for an offline caller — `naozhi
// config check --effective` — describing what the spawn path would pass for a
// session of agent on backend bd under access profile profileID. Same
// function, same chain; the only tier absent is session tuning, which is a
// live dashboard pick and not part of any config. Before this existed the
// check kept a second copy of the precedence and it drifted on two points
// (agent args replaced instead of appended; default_model ignored) (#2969).
func EffectiveArgvLayers(bd BackendDefaults, profiles map[string]AccessProfile, profileID string, agent AgentOpts) (model, effort string, args []string, systemPrompt string) {
	merged := mergeArgvLayers(bd, profileDefaultModelFor(profiles, profileID), shim.SpawnOverlay{
		Model:              agent.Model,
		Effort:             agent.Effort,
		ExtraArgs:          agent.ExtraArgs,
		AccessProfile:      profileID,
		AppendSystemPrompt: agent.SystemPrompt,
	}, "", "")
	return merged.Model, merged.Effort, merged.Args, merged.SystemPrompt
}
