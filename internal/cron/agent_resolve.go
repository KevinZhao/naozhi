package cron

import "github.com/naozhi/naozhi/internal/agentroute"

// resolveAgent maps a "/agent rest..." style prompt to the agent ID
// configured in agentCommands; returns ("general", text) on no-prefix
// or unrecognised command. Delegates to internal/agentroute (shared with
// session.ResolveAgent) so cron carries no import edge onto
// internal/session (#2194).
func resolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
	return agentroute.ResolveAgent(text, agentCommands)
}
