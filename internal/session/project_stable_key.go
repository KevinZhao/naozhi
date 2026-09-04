package session

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// projectStableKeyHashLen is the hex length of the workspace-path hash
// embedded in a project-stable key. 16 hex chars = 64 bits of SHA-256.
// Birthday-paradox collision probability for n single-host projects is
// ~n^2 / 2^65 (≈2.7e-12 at n=1e4), which the RFC accepts without
// mitigation (docs/rfc/project-stable-session-key.md §4.1 / §6).
const projectStableKeyHashLen = 16

// ProjectStableKey returns the canonical project-level stable dashboard
// session key for a workspace. The shape is
//
//	dashboard:pj:<sha256(filepath.Clean(absPath))[:16]>:<agent>
//
// The same (absPath, agent) always yields the same key, so the chain it
// carries survives restarts; filepath.Clean makes "/a/" and "/a" one key. The
// hash covers the FULL absolute path, so "/x/foo" and "/y/foo" do not collide
// (RFC §4.1). agent goes through the usual key-component gate; empty agent
// falls back to "general". Returns "" when absPath is empty.
func ProjectStableKey(absPath, agent string) string {
	if absPath == "" {
		return ""
	}
	clean := filepath.Clean(absPath)
	sum := sha256.Sum256([]byte(clean))
	hash := hex.EncodeToString(sum[:])[:projectStableKeyHashLen]

	a := sanitizeKeyComponent(agent)
	if a == "" {
		a = "general"
	}
	return sessionkey.DashboardPlatform + ":" +
		sessionkey.DashboardProjectChatType + ":" +
		hash + ":" + a
}
