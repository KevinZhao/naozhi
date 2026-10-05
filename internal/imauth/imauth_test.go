package imauth

import "testing"

func set(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func TestDecide(t *testing.T) {
	open := &Policy{Rules: map[string]Rule{}}
	closed := &Policy{DefaultDeny: true}
	listed := &Policy{Rules: map[string]Rule{
		"feishu": {Allowed: set("ou_alice", "ou_bob"), Admins: set("ou_root")},
	}}
	noAdmins := &Policy{Rules: map[string]Rule{
		"slack": {Allowed: set("U1")},
	}}
	adminsOnly := &Policy{Rules: map[string]Rule{
		"discord": {Admins: set("42")},
	}}
	empty := &Policy{Rules: map[string]Rule{"weixin": {}}}

	cases := []struct {
		name     string
		p        *Policy
		platform string
		user     string
		class    Class
		wantOK   bool
		wantWhy  string
	}{
		{"nil policy allows chat", nil, "feishu", "anyone", Chat, true, ""},
		{"nil policy allows admin", nil, "feishu", "", Admin, true, ""},
		{"no rule allows", open, "feishu", "anyone", Admin, true, ""},
		{"no rule allows empty user", open, "feishu", "", Chat, true, ""},
		{"default_deny refuses unruled platform", closed, "feishu", "anyone", Chat, false, ReasonDefaultDeny},
		{"default_deny refuses empty user too", closed, "slack", "", Chat, false, ReasonDefaultDeny},
		{"rule on another platform leaves this one open", listed, "slack", "anyone", Admin, true, ""},
		{"allowed user chats", listed, "feishu", "ou_alice", Chat, true, ""},
		{"allowed user is not admin", listed, "feishu", "ou_alice", Admin, false, ReasonNotAdmin},
		{"admin chats", listed, "feishu", "ou_root", Chat, true, ""},
		{"admin runs admin command", listed, "feishu", "ou_root", Admin, true, ""},
		{"unlisted user refused", listed, "feishu", "ou_eve", Chat, false, ReasonNotAllowed},
		{"unlisted user refused admin", listed, "feishu", "ou_eve", Admin, false, ReasonNotAllowed},
		{"empty user refused once a rule exists", listed, "feishu", "", Chat, false, ReasonEmptyUser},
		{"ids compare exactly", listed, "feishu", "OU_ALICE", Chat, false, ReasonNotAllowed},
		{"empty admins makes allowed users admins", noAdmins, "slack", "U1", Admin, true, ""},
		{"empty admins still refuses strangers", noAdmins, "slack", "U2", Admin, false, ReasonNotAllowed},
		{"admins-only rule refuses others' chat", adminsOnly, "discord", "7", Chat, false, ReasonNotAllowed},
		{"admins-only rule lets admin chat", adminsOnly, "discord", "42", Chat, true, ""},
		{"empty rule refuses everyone", empty, "weixin", "w1", Chat, false, ReasonNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := tc.p.Decide(tc.platform, tc.user, tc.class)
			if ok != tc.wantOK || why != tc.wantWhy {
				t.Errorf("Decide(%q, %q, %v) = (%v, %q), want (%v, %q)",
					tc.platform, tc.user, tc.class, ok, why, tc.wantOK, tc.wantWhy)
			}
		})
	}
}

func TestClassString(t *testing.T) {
	if Chat.String() != "chat" || Admin.String() != "admin" {
		t.Errorf("Class names = %q, %q", Chat.String(), Admin.String())
	}
}
