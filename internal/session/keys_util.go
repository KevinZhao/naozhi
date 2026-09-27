package session

import (
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// SanitizeLogAttr returns a version of s that is safe to feed directly into
// slog attributes without fragmenting log lines (same rules as session-key
// components). Call it on any IM-originated string (chat ID, user ID, raw
// incoming key) BEFORE passing it to slog so an attacker-controlled ID
// cannot inject \n, tabs, or ANSI into operator log streams.
func SanitizeLogAttr(s string) string {
	return sanitizeKeyComponent(s)
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return sanitizeKeyComponent(platform) + ":" + sanitizeKeyComponent(chatType) + ":" + sanitizeKeyComponent(id) + ":" + sanitizeKeyComponent(agentID)
}

// sanitizeKeyComponent truncates and strips colons from a session key
// component; see sessionkey.SanitizeKeyComponent.
func sanitizeKeyComponent(s string) string { return sessionkey.SanitizeKeyComponent(s) }

// SanitizeCWDKey converts a filesystem path to a safe session-key component;
// see sessionkey.SanitizeCWDKey.
func SanitizeCWDKey(cwd string) string { return sessionkey.SanitizeCWDKey(cwd) }

// TakeoverKey builds a session key for a takeover from a discovered process
// CWD; see sessionkey.TakeoverKey.
func TakeoverKey(cwdKey string) string { return sessionkey.TakeoverKey(cwdKey) }
