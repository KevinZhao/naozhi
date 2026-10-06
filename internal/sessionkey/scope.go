package sessionkey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Scope kinds a group chat's sessions can be split by (ScopedChatID).
const (
	ScopeThread = "t"
	ScopeUser   = "u"
)

// scopeGroupChatType is the only chat type whose chat segment can carry a
// scope. Other chat IDs are never scoped, and a takeover's path-derived
// segment may itself contain '#'.
const scopeGroupChatType = "group"

// scopedIDHashHex is how many hex digits of sha256(id) replace an id that does
// not fit in the chat segment.
const scopedIDHashHex = 16

// ScopedChatID returns the chat segment of the session key for one thread or
// member of a group chat: "chatID#<kind><id>". An empty id returns chatID. An
// id that would push the segment past MaxKeyComponent is replaced by a prefix
// of its sha256, so SanitizeKeyComponent never truncates two scopes into one;
// if even that does not fit, chatID is returned unscoped.
func ScopedChatID(chatID, kind, id string) string {
	if id == "" {
		return chatID
	}
	s := chatID + "#" + kind + id
	if len(s) <= MaxKeyComponent {
		return s
	}
	sum := sha256.Sum256([]byte(id))
	s = chatID + "#" + kind + hex.EncodeToString(sum[:])[:scopedIDHashHex]
	if len(s) <= MaxKeyComponent {
		return s
	}
	return chatID
}

// ParentChatID strips a ScopedChatID scope from a group chat's chatID. Other
// chat types are returned as they are.
func ParentChatID(chatType, chatID string) string {
	if chatType != scopeGroupChatType {
		return chatID
	}
	if i := strings.IndexByte(chatID, '#'); i >= 0 {
		return chatID[:i]
	}
	return chatID
}

// ParentChatKey is ParentChatID applied to a ChatKey
// ("platform:chatType:chatID"): a thread's or member's chat key maps to its
// chat's. Anything else is returned as it is.
func ParentChatKey(chatKey string) string {
	platform, rest, ok := strings.Cut(chatKey, ":")
	if !ok {
		return chatKey
	}
	chatType, chatID, ok := strings.Cut(rest, ":")
	if !ok || strings.IndexByte(chatID, ':') >= 0 {
		return chatKey
	}
	if parent := ParentChatID(chatType, chatID); parent != chatID {
		return platform + ":" + chatType + ":" + parent
	}
	return chatKey
}
