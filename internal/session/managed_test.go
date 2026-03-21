package session

import "testing"

func TestSessionKey(t *testing.T) {
	tests := []struct {
		platform, chatType, id, agentID string
		expected                        string
	}{
		{"feishu", "direct", "alice", "general", "feishu:direct:alice:general"},
		{"feishu", "group", "xxx", "code-reviewer", "feishu:group:xxx:code-reviewer"},
		{"feishu", "direct", "bob", "", "feishu:direct:bob:general"},
		{"telegram", "direct", "user1", "researcher", "telegram:direct:user1:researcher"},
	}

	for _, tt := range tests {
		got := SessionKey(tt.platform, tt.chatType, tt.id, tt.agentID)
		if got != tt.expected {
			t.Errorf("SessionKey(%q,%q,%q,%q) = %q, want %q",
				tt.platform, tt.chatType, tt.id, tt.agentID, got, tt.expected)
		}
	}
}
