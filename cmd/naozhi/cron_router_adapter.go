// cron_router_adapter.go retains the boot-time session.AgentOpts →
// cron.AgentOpts projection used to build the cron Scheduler's agents map.
//
// R260528-ARCH-23 (#1382): the cron.SessionRouter / cron.Session adapters and
// the ordinal init() pin moved to internal/wireup (the layer that already
// imports both cron and session and now builds the adapter from
// SchedulersDeps.Router). Only toCronAgentOpts stays here — it is consumed by
// buildAgentOpts (main_init.go) to construct the cron-local agents map at
// boot, a main-package concern unrelated to the router adapter seam.

package main

import (
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// toCronAgentOpts copies session.AgentOpts → cron.AgentOpts; used to build the
// cron.Scheduler's agents map at boot from cfg.Agents (via buildAgentOpts).
//
// ExtraArgs is cloned (not aliased): cron's Scheduler stores AgentOpts in a map
// that is read once-and-treated-as-immutable, but cloning closes the same
// aliasing contract the router-feed path observes (session/router_lifecycle.go
// ExtraArgs ownership note).
func toCronAgentOpts(o session.AgentOpts) cron.AgentOpts {
	out := cron.AgentOpts{
		Model:     o.Model,
		Workspace: o.Workspace,
		Backend:   o.Backend,
		Exempt:    o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}
