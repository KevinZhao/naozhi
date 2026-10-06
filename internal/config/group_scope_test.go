package config

import (
	"strings"
	"testing"
)

// TestLoad_GroupScope: absent is thread; each scope loads as written.
func TestLoad_GroupScope(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"", GroupScopeThread},
		{"session:\n  group_scope: thread\n", GroupScopeThread},
		{"session:\n  group_scope: chat\n", GroupScopeChat},
		{"session:\n  group_scope: user\n", GroupScopeUser},
	} {
		cfg, err := Load(writeCfg(t, tc.body))
		if err != nil {
			t.Fatalf("Load(%q): %v", tc.body, err)
		}
		if got := cfg.Session.GroupScope; got != tc.want {
			t.Errorf("Load(%q).Session.GroupScope = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestLoad_GroupScopeRefusesUnknown: a misspelled scope is fatal rather than
// silently treated as one of the three.
func TestLoad_GroupScopeRefusesUnknown(t *testing.T) {
	_, err := Load(writeCfg(t, "session:\n  group_scope: threads\n"))
	if err == nil || !strings.Contains(err.Error(), `session.group_scope must be "thread", "chat" or "user", got "threads"`) {
		t.Fatalf("Load(group_scope: threads) err = %v, want the group_scope refusal", err)
	}
}

// TestUnknownKeys_GroupScopeIsKnown: session.group_scope is a key the config
// has, so it is not reported as unknown.
func TestUnknownKeys_GroupScopeIsKnown(t *testing.T) {
	diags, _, err := collectLoadDiags(t, writeCfg(t, "session:\n  group_scope: chat\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, d := range diags {
		if strings.Contains(d.Key, "group_scope") || strings.Contains(d.Reason, "group_scope") {
			t.Errorf("session.group_scope reported: %+v", d)
		}
	}
}
