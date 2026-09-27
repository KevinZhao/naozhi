package session

import (
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// SanitizeLogAttr makes s safe as a slog attribute; see
// sessionkey.SanitizeLogAttr.
func SanitizeLogAttr(s string) string { return sessionkey.SanitizeLogAttr(s) }

// SessionKey builds a session key from components; see sessionkey.SessionKey.
func SessionKey(platform, chatType, id, agentID string) string {
	return sessionkey.SessionKey(platform, chatType, id, agentID)
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
