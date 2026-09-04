package session

// JoinSystemPrompts layers extra on top of base for AgentOpts.SystemPrompt:
// base first, then extra, separated by a blank line. Either side may be
// empty, in which case the other is returned unchanged (so a lone layer never
// grows a stray separator).
//
// This is the single stacking rule for every prompt channel (#2493):
//
//	agents[<id>].system_prompt          ← base (config, per agent)
//	  + project planner prompt          ← ResolveForChat / buildSessionOpts
//	  + scratch quoted context          ← ScratchPool.Open
//
// Order matters: the agent's standing instructions come first, the
// situational context (what project / what quote) after, mirroring how the
// CLI's own system prompt precedes the appended text. Callers never mutate a
// shared AgentOpts to add a layer — they copy and assign the joined result —
// so the agent registry map value stays untouched (same contract as the
// ExtraArgs three-arg-slice clone).
func JoinSystemPrompts(base, extra string) string {
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	}
	return base + "\n\n" + extra
}
