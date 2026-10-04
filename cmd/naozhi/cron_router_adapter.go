// Boot-time session.AgentOpts → cron.AgentOpts projection; the router adapters
// themselves live in internal/wireup.

package main

import (
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// toCronAgentOpts copies session.AgentOpts → cron.AgentOpts. ExtraArgs is
// cloned, not aliased, matching the router-feed path's ownership contract.
// The agent's DefaultBackend becomes cron's Backend when none is set: cron has
// no picker, and a job's own backend still overrides it at run time.
func toCronAgentOpts(o session.AgentOpts) cron.AgentOpts {
	backend := o.Backend
	if backend == "" {
		backend = o.DefaultBackend
	}
	out := cron.AgentOpts{
		Model:        o.Model,
		Workspace:    o.Workspace,
		Backend:      backend,
		Effort:       o.Effort,
		SystemPrompt: o.SystemPrompt,
		Exempt:       o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}
