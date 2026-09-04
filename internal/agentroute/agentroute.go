// Package agentroute is the single source of truth for parsing a
// "/command rest..." style prompt into an agent ID + clean text. It is a
// leaf package (imports only "strings") so both internal/session and
// internal/cron can depend on it without an import edge between them (#2194).
package agentroute

import "strings"

// ResolveAgent maps a "/command rest..." prompt to the agent ID configured in
// agentCommands, returning ("general", text) on no-prefix or unrecognised
// command. The command token match is case-insensitive so a mobile IME
// auto-capitalizing "/Review" still routes; agentCommands keys are lowercase.
func ResolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
	if !strings.HasPrefix(text, "/") {
		return "general", text
	}
	parts := strings.SplitN(text, " ", 2)
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}
	if id, ok := agentCommands[cmd]; ok {
		return id, rest
	}
	return "general", text
}
