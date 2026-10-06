package workflow

import "slices"

// AgentEqualIgnoringRev reports whether two rows have the same content: the
// board keeps a row's Rev when it is. Agent holds a slice, so it is not
// comparable with ==; a test pins the field count so a new field cannot be
// left out here.
func AgentEqualIgnoringRev(a, b *Agent) bool {
	return a.Index == b.Index &&
		a.PhaseIndex == b.PhaseIndex &&
		a.Label == b.Label &&
		a.AgentID == b.AgentID &&
		slices.Equal(a.PrevAgentIDs, b.PrevAgentIDs) &&
		a.Model == b.Model &&
		a.State == b.State &&
		a.RawState == b.RawState &&
		a.Blocked == b.Blocked &&
		a.Attempt == b.Attempt &&
		a.Cached == b.Cached &&
		a.QueuedAt == b.QueuedAt &&
		a.StartedAt == b.StartedAt &&
		a.LastProgressAt == b.LastProgressAt &&
		a.DurationMS == b.DurationMS &&
		a.Tokens == b.Tokens &&
		a.ToolCalls == b.ToolCalls &&
		a.LastTool == b.LastTool &&
		a.LastToolSummary == b.LastToolSummary &&
		a.Error == b.Error
}
