package session

import "github.com/naozhi/naozhi/internal/agentroute"

// ResolveAgent parses a /command prefix and returns the agent ID and clean
// text. Thin delegate to the single source of truth in internal/agentroute
// (R202606b-ARCH-5, #2194); kept as an exported wrapper so existing callers
// (dispatch.go) need no edit.
func ResolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
	return agentroute.ResolveAgent(text, agentCommands)
}
