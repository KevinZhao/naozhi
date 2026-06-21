package cron

import "github.com/naozhi/naozhi/internal/agentroute"

// resolveAgent maps a "/agent rest..." style prompt to the agent ID
// configured in agentCommands; returns ("general", text) on no-prefix
// or unrecognised command.
//
// R202606b-ARCH-5 (#2194): formerly a byte-for-byte fork of
// session.ResolveAgent, kept in sync only by a hand-enumerated parity test.
// Both now delegate to the single source of truth in internal/agentroute, a
// leaf package importing only "strings" — so the fork (and its drift risk) is
// gone while the cron package still carries zero production import edge onto
// internal/session. Kept as an unexported wrapper so scheduler_run.go is
// untouched.
func resolveAgent(text string, agentCommands map[string]string) (agentID, cleanText string) {
	return agentroute.ResolveAgent(text, agentCommands)
}
