package workflow

import (
	"slices"
	"sort"
)

// WireView is a Workflow as it goes on the wire: every json-tagged field of
// Workflow (a test pins the correspondence) with Agents holding only the
// rows a frame or response carries. The WS frame and the HTTP response use it.
type WireView struct {
	TaskID         string  `json:"task_id"`
	RunID          string  `json:"run_id,omitempty"`
	Name           string  `json:"name,omitempty"`
	Description    string  `json:"description,omitempty"`
	Current        string  `json:"current,omitempty"`
	Status         Status  `json:"status"`
	RawStatus      string  `json:"raw_status,omitempty"`
	StartedAt      int64   `json:"started_at,omitempty"`
	EndedAt        int64   `json:"ended_at,omitempty"`
	LastObservedAt int64   `json:"last_observed_at,omitempty"`
	Tokens         int64   `json:"tokens,omitempty"`
	ToolCalls      int     `json:"tool_calls,omitempty"`
	DurationMS     int64   `json:"duration_ms,omitempty"`
	Counts         Counts  `json:"counts"`
	Phases         []Phase `json:"phases"`
	Agents         []Agent `json:"agents"`
	AgentsCapped   bool    `json:"agents_capped,omitempty"`
	NotifySummary  string  `json:"notify_summary,omitempty"`
	Source         Source  `json:"source"`
	Degraded       string  `json:"degraded,omitempty"`
	Version        uint64  `json:"version"`
}

// Wire returns w's header and phases with rows as its agents. Nil phases
// and rows encode as empty arrays, never null. Strings are already redacted.
func (w *Workflow) Wire(rows []Agent) WireView {
	phases := w.Phases
	if phases == nil {
		phases = []Phase{}
	}
	if rows == nil {
		rows = []Agent{}
	}
	return WireView{
		TaskID: w.TaskID, RunID: w.RunID, Name: w.Name, Description: w.Description,
		Current: w.Current, Status: w.Status, RawStatus: w.RawStatus,
		StartedAt: w.StartedAt, EndedAt: w.EndedAt, LastObservedAt: w.LastObservedAt,
		Tokens: w.Tokens, ToolCalls: w.ToolCalls, DurationMS: w.DurationMS,
		Counts: w.Counts, Phases: phases, Agents: rows, AgentsCapped: w.AgentsCapped,
		NotifySummary: w.NotifySummary, Source: w.Source, Degraded: w.Degraded, Version: w.Version,
	}
}

// RowsAfter returns the rows of rows whose content changed after wire
// version v: what a delta based on v carries.
func RowsAfter(rows []Agent, v uint64) []Agent {
	var out []Agent
	for i := range rows {
		if rows[i].Rev > v {
			out = append(out, rows[i])
		}
	}
	return out
}

// AgentLoc locates a workflow agent by agentId.
type AgentLoc struct {
	TaskID string
	Index  int
	// Current is false when the id is one of the row's PrevAgentIDs.
	Current bool
}

// Published is what a session's workflow board publishes: immutable, read
// lock-free by snapshots, the WS hub and HTTP. Build it with NewPublished.
type Published struct {
	Epoch     string      // board epoch, 16 hex digits
	Workflows []*Workflow // unsettled first, then terminal, newest first
	byAgentID map[string]AgentLoc
}

// NewPublished orders wfs (taking ownership) and indexes their agentIds,
// current and earlier attempts; an id in two places goes to its current
// attempt, else to the first. It reuses prev's index, shared read-only,
// when that maps every agentId to the same place.
func NewPublished(epoch string, wfs []*Workflow, prev *Published) *Published {
	sortWorkflows(wfs)
	p := &Published{Epoch: epoch, Workflows: wfs}
	if prev != nil && prev.byAgentID != nil && sameAgentIndex(wfs, prev.byAgentID) {
		p.byAgentID = prev.byAgentID
		return p
	}
	p.byAgentID = map[string]AgentLoc{}
	eachAgentLoc(wfs, func(id string, loc AgentLoc) bool {
		if old, dup := p.byAgentID[id]; !dup || (loc.Current && !old.Current) {
			p.byAgentID[id] = loc
		}
		return true
	})
	return p
}

// eachAgentLoc yields every (agentId, location) of wfs in order.
func eachAgentLoc(wfs []*Workflow, yield func(string, AgentLoc) bool) {
	for _, w := range wfs {
		for i := range w.Agents {
			a := &w.Agents[i]
			for _, id := range a.PrevAgentIDs {
				if id != a.AgentID && !yield(id, AgentLoc{TaskID: w.TaskID, Index: a.Index}) {
					return
				}
			}
			if a.AgentID != "" && !yield(a.AgentID, AgentLoc{TaskID: w.TaskID, Index: a.Index, Current: true}) {
				return
			}
		}
	}
}

// sameAgentIndex reports whether idx is the index of wfs, without
// building one. A duplicated id yields two places, which cannot both match.
func sameAgentIndex(wfs []*Workflow, idx map[string]AgentLoc) bool {
	n, ok := 0, true
	eachAgentLoc(wfs, func(id string, loc AgentLoc) bool {
		n++
		ok = idx[id] == loc
		return ok
	})
	return ok && n == len(idx)
}

// Agent finds an agentId's row; false on a nil Published or an unknown id.
func (p *Published) Agent(agentID string) (AgentLoc, bool) {
	if p == nil {
		return AgentLoc{}, false
	}
	loc, ok := p.byAgentID[agentID]
	return loc, ok
}

// AgentState is the state of loc's row. An earlier attempt's id reports
// done: that attempt is over whatever the row now shows.
func (p *Published) AgentState(loc AgentLoc) (AgentState, bool) {
	if p == nil {
		return "", false
	}
	i := slices.IndexFunc(p.Workflows, func(w *Workflow) bool { return w.TaskID == loc.TaskID })
	if i < 0 {
		return "", false
	}
	rows := p.Workflows[i].Agents
	j := sort.Search(len(rows), func(k int) bool { return rows[k].Index >= loc.Index })
	if j == len(rows) || rows[j].Index != loc.Index {
		return "", false
	}
	if !loc.Current {
		return AgentDone, true
	}
	return rows[j].State, true
}
