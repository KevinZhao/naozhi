package wireup

import (
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/reader"
)

// LookupDeliver is reader.Config.Deliver over the Planner: a lookup's
// answer as a turn of its own from agent.SourceLookup, which may only
// notify, promise and ask.
func LookupDeliver(a *agent.Agent) func(block string) error {
	return func(block string) error { return a.Wake(agent.SourceLookup, nil, block) }
}

// PlannerLookup is action.Config.Lookup over the readers. A question asked
// today, or running, starts no run; its answer still comes as a turn of its
// own, which says so, so the receipt says nothing of it.
func PlannerLookup(r *reader.Reader) func(q, why string) error {
	return func(q, why string) error {
		_, err := r.Lookup(q, why)
		return err
	}
}
