package sessionkey

// SessionKey builds a session key from components; an empty agentID means
// "general". Every component goes through SanitizeKeyComponent.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return SanitizeKeyComponent(platform) + ":" + SanitizeKeyComponent(chatType) + ":" + SanitizeKeyComponent(id) + ":" + SanitizeKeyComponent(agentID)
}

// ChatKey builds a chat-level key (without agent suffix) for workspace
// overrides. SECURITY: components are sanitized with the same rule as
// SessionKey so a malicious chat ID with C0/ANSI bytes or Unicode bidi
// overrides cannot inject fabricated slog.TextHandler log lines.
func ChatKey(platform, chatType, chatID string) string {
	return SanitizeKeyComponent(platform) + ":" + SanitizeKeyComponent(chatType) + ":" + SanitizeKeyComponent(chatID)
}

// SanitizeLogAttr returns a version of s that is safe to feed directly into
// slog attributes without fragmenting log lines (same rules as session-key
// components). Call it on any IM-originated string (chat ID, user ID, raw
// incoming key) BEFORE passing it to slog so an attacker-controlled ID
// cannot inject \n, tabs, or ANSI into operator log streams.
func SanitizeLogAttr(s string) string {
	return SanitizeKeyComponent(s)
}
