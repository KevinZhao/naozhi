package cron

import "strings"

// resolveAgent maps a "/agent rest..." style prompt to the agent ID
// configured in agentCommands; returns ("general", text) on no-prefix
// or unrecognised command.
//
// Cron-local copy of internal/session.ResolveAgent — the function is a
// pure value-only helper (no shared state, no imports beyond strings)
// and inlining it removes the last cron → session production import
// edge that wasn't a reverse interface implementation. session also
// keeps its own copy for dispatch / server callers; if either copy
// changes semantics, the test in scheduler_test.go pins the cron
// behaviour.
func resolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
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
