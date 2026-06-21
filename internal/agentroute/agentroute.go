// Package agentroute is the single source of truth for parsing a
// "/command rest..." style prompt into an agent ID + clean text.
//
// R202606b-ARCH-5 (#2194): this logic previously lived as two byte-for-byte
// forks — session.ResolveAgent (IM/dispatch callers) and cron.resolveAgent
// (cron jobs) — kept in sync only by a hand-enumerated parity test. The fork
// existed so the cron package carried zero production import edge onto
// internal/session. A fresh leaf package importing only "strings", depended on
// by both, removes the fork without re-introducing that edge (agentroute
// imports neither, so no cycle is possible). session/cron keep thin exported/
// unexported delegates so their callers are untouched.
package agentroute

import "strings"

// ResolveAgent maps a "/command rest..." prompt to the agent ID configured in
// agentCommands, returning ("general", text) on no-prefix or unrecognised
// command. The command token match is case-insensitive so that a CJK mobile
// IME auto-capitalizing the first letter (e.g. "/Review") still routes
// correctly. Agent registration keys in agentCommands are assumed lowercase.
//
// Pure value-only helper: no shared state, no imports beyond strings.
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
