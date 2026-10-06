package sessionkey

import (
	"strings"
	"testing"
)

// TestScopedChatID_Compose: a thread or member narrows the chat segment, and
// ParentChatID gives the chat back; no scope leaves the chat's key as it was.
func TestScopedChatID_Compose(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, id, want string
	}{
		{ScopeThread, "1700000000.000042", "C1#t1700000000.000042"},
		{ScopeUser, "U7", "C1#uU7"},
		{ScopeThread, "", "C1"},
		{ScopeUser, "", "C1"},
	} {
		got := ScopedChatID("C1", tc.kind, tc.id)
		if got != tc.want {
			t.Errorf("ScopedChatID(C1, %q, %q) = %q, want %q", tc.kind, tc.id, got, tc.want)
		}
		if p := ParentChatID("group", got); p != "C1" {
			t.Errorf("ParentChatID(group, %q) = %q, want C1", got, p)
		}
	}
	if got := SessionKey("slack", "group", ScopedChatID("C1", ScopeThread, ""), "general"); got != "slack:group:C1:general" {
		t.Errorf("unthreaded key = %q, want the unchanged slack:group:C1:general", got)
	}
}

// TestScopedChatID_LongIDIsHashedNotTruncated: two long ids that share their
// first 128 bytes get two keys, each a valid 4-segment session key with its
// chat as parent.
func TestScopedChatID_LongIDIsHashedNotTruncated(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("x", MaxKeyComponent)
	a := ScopedChatID("oc_chat", ScopeThread, base+"a")
	b := ScopedChatID("oc_chat", ScopeThread, base+"b")
	if a == b {
		t.Fatalf("two long ids share the chat segment %q", a)
	}
	for _, s := range []string{a, b} {
		if len(s) > MaxKeyComponent {
			t.Errorf("ScopedChatID = %q is %d bytes, over %d", s, len(s), MaxKeyComponent)
		}
		if !strings.HasPrefix(s, "oc_chat#t") {
			t.Errorf("ScopedChatID = %q, want the oc_chat#t prefix", s)
		}
		key := SessionKey("feishu", "group", s, "general")
		if err := ValidateSessionKey(key); err != nil {
			t.Errorf("ValidateSessionKey(%q): %v", key, err)
		}
		if n := strings.Count(key, ":"); n != 3 {
			t.Errorf("key %q has %d separators, want 3", key, n)
		}
		if got := ParentChatKey(strings.TrimSuffix(key, ":general")); got != "feishu:group:oc_chat" {
			t.Errorf("ParentChatKey of %q = %q, want feishu:group:oc_chat", key, got)
		}
	}
	if again := ScopedChatID("oc_chat", ScopeThread, base+"a"); again != a {
		t.Errorf("hashed scope is not stable: %q then %q", a, again)
	}
}

// TestScopedChatID_ChatTooLongStaysUnscoped: when not even the hashed scope
// fits, the session falls back to the chat's rather than a truncated key.
func TestScopedChatID_ChatTooLongStaysUnscoped(t *testing.T) {
	t.Parallel()
	chat := strings.Repeat("c", MaxKeyComponent-2)
	if got := ScopedChatID(chat, ScopeThread, "T1"); got != chat {
		t.Errorf("ScopedChatID(long chat) = %q, want the chat unscoped", got)
	}
}

// TestParentChatKey: only a group chat's segment carries a scope; a direct
// chat, a takeover's path-derived segment or a planner key keep their '#'.
func TestParentChatKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"slack:group:C1#t1700.1", "slack:group:C1"},
		{"feishu:group:oc_1#uou_2", "feishu:group:oc_1"},
		{"slack:group:C1", "slack:group:C1"},
		{"slack:direct:D1#t1700.1", "slack:direct:D1#t1700.1"},
		{"local:takeover:Users-me-repo#2", "local:takeover:Users-me-repo#2"},
		{"project:demo:planner", "project:demo:planner"},
		{"cron:job1", "cron:job1"},
		{"nocolon", "nocolon"},
		{"a:group:b:c#t1", "a:group:b:c#t1"},
	} {
		if got := ParentChatKey(tc.in); got != tc.want {
			t.Errorf("ParentChatKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := ParentChatID("direct", "D1#t1"); got != "D1#t1" {
		t.Errorf("ParentChatID(direct) = %q, want it unchanged", got)
	}
}
