package sessionkey

import (
	"strings"
	"testing"
)

// TestChatKey_ComponentsCannotForgeSegments: an IM-supplied component with a
// colon or control bytes cannot add a key segment or reach a log line raw.
func TestChatKey_ComponentsCannotForgeSegments(t *testing.T) {
	t.Parallel()
	got := ChatKey("feishu", "group", "evil:direct:x\n\x1b[31m")
	if n := strings.Count(got, ":"); n != 2 {
		t.Errorf("ChatKey = %q has %d separators, want 2", got, n)
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Errorf("ChatKey = %q carries control byte %#x", got, r)
		}
	}
}

// TestChatKey_IsTheSessionKeyPrefix: a chat's key is exactly the prefix of
// every session key in that chat, whatever the components contain; resetting
// a chat matches its sessions by that prefix.
func TestChatKey_IsTheSessionKeyPrefix(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ platform, chatType, chatID, agent string }{
		{"feishu", "group", "oc_123", "general"},
		{"slack", "direct", "U1:with:colons", "reviewer"},
		{"weixin", "group", "chat\u202eid\x07", ""},
	} {
		chat := ChatKey(c.platform, c.chatType, c.chatID)
		sess := SessionKey(c.platform, c.chatType, c.chatID, c.agent)
		if !strings.HasPrefix(sess, chat+":") {
			t.Errorf("SessionKey = %q does not start with ChatKey %q + \":\"", sess, chat)
		}
	}
}

// TestSessionKey_EmptyAgentIsGeneral: an unnamed agent routes to "general".
func TestSessionKey_EmptyAgentIsGeneral(t *testing.T) {
	t.Parallel()
	if got := SessionKey("feishu", "direct", "u1", ""); got != "feishu:direct:u1:general" {
		t.Errorf("SessionKey(empty agent) = %q, want feishu:direct:u1:general", got)
	}
}
