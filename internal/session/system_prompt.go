package session

// JoinSystemPrompts layers extra on top of base for AgentOpts.SystemPrompt,
// separated by a blank line; an empty side returns the other unchanged. It is
// the single stacking rule for every prompt channel (#2493):
//
//	agents[<id>].system_prompt          ← base (config, per agent)
//	  + project planner prompt          ← ResolveForChat / buildSessionOpts
//	  + scratch quoted context          ← ScratchPool.Open
//
// Callers never mutate a shared AgentOpts to add a layer — they copy and
// assign the joined result — so the agent registry map value stays untouched.
func JoinSystemPrompts(base, extra string) string {
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	}
	return base + "\n\n" + extra
}
