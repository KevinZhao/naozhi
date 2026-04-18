package session

import "strings"

// ResolveAgent parses a /command prefix and returns the agent ID and clean text.
func ResolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
	if !strings.HasPrefix(text, "/") {
		return "general", text
	}
	parts := strings.SplitN(text, " ", 2)
	cmd := strings.TrimPrefix(parts[0], "/")
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}
	if id, ok := agentCommands[cmd]; ok {
		return id, rest
	}
	return "general", text
}
