package session

import "github.com/naozhi/naozhi/internal/agentroute"

// ResolveAgent parses a /command prefix and returns the agent ID and clean
// text. Thin delegate to internal/agentroute (#2194).
func ResolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
	return agentroute.ResolveAgent(text, agentCommands)
}
