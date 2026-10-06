// resolver.go builds the session.KeyResolver the server (dispatcher, Hub,
// handlers) and the upstream connector share; composing it reads cron, project
// and session together, so it lives in the composition root.

package wireup

import (
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// KeyResolver returns the process's one KeyResolver over agents and projects.
// With a scheduler, a cron key resolves to the access profile of the agent its
// job's prompt routes to via agentCommands (the maps the scheduler was built
// from), so the remote-dispatch gate holds a profile-pinned cron run local.
// nil projects disables project routing; nil sched leaves cron keys at "".
func KeyResolver(agents map[string]session.AgentOpts, agentCommands map[string]string, projects *project.Manager, sched *cron.Scheduler) *session.KeyResolver {
	resolver := session.NewKeyResolver(agents, project.NewDataSource(projects))
	if sched == nil {
		return resolver
	}
	return resolver.WithCronAccessProfile(func(jobID string) string {
		j, ok := sched.GetJob(jobID)
		if !ok {
			return ""
		}
		agentID, _ := session.ResolveAgent(j.Prompt, agentCommands)
		return agents[agentID].AccessProfile
	})
}
