package session

import (
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// SessionKey builds a session key from components; see sessionkey.SessionKey.
func SessionKey(platform, chatType, id, agentID string) string {
	return sessionkey.SessionKey(platform, chatType, id, agentID)
}

// sanitizeKeyComponent truncates and strips colons from a session key
// component; see sessionkey.SanitizeKeyComponent.
func sanitizeKeyComponent(s string) string { return sessionkey.SanitizeKeyComponent(s) }
