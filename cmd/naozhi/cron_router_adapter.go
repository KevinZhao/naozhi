// Boot-time session.AgentOpts → cron.AgentOpts projection; the router adapters
// themselves live in internal/wireup.

package main

import (
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// toCronAgentOpts copies session.AgentOpts → cron.AgentOpts. ExtraArgs is
// cloned, not aliased, matching the router-feed path's ownership contract.
func toCronAgentOpts(o session.AgentOpts) cron.AgentOpts {
	out := cron.AgentOpts{
		Model:        o.Model,
		Workspace:    o.Workspace,
		Backend:      o.Backend,
		Effort:       o.Effort,
		SystemPrompt: o.SystemPrompt,
		Exempt:       o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}
