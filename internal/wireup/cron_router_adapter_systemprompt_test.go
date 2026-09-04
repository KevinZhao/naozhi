package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestToSessionAgentOpts_SystemPrompt pins the #2493 field across the cron →
// session hop: a cron job for an agent with agents[].system_prompt must spawn
// with that prompt like any other session of the agent.
func TestToSessionAgentOpts_SystemPrompt(t *testing.T) {
	t.Parallel()
	out := toSessionAgentOpts(cron.AgentOpts{Model: "opus", SystemPrompt: "You are a code review expert."})
	if out.SystemPrompt != "You are a code review expert." {
		t.Errorf("SystemPrompt not propagated: %+v", out)
	}
}
