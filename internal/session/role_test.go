package session

import "testing"

// TestClassifyKey covers each branch of classifyKey, the pure function
// behind ManagedSession.Role(). R222-ARCH-10 (#728) introduced the
// SessionRole enum so callers don't have to spell out the
// `IsCronKey || IsScratchKey || ...` chain at every site; this test
// pins the prefix → role mapping so a future namespace addition can't
// silently land without a Role() update.
func TestClassifyKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want SessionRole
	}{
		{"cron stub", "cron:hourly-job", RoleCron},
		{"scratch", "scratch:abc:general:general", RoleScratch},
		{"sys daemon", "sys:auto-titler", RoleSys},
		{"project planner", "project:naozhi:planner", RoleProject},
		{"global planner", "planner:main", RolePlanner},
		{"plain IM", "feishu:c2c:U_oc_abc:general", RoleIM},
		{"empty defaults to IM", "", RoleIM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyKey(tc.key); got != tc.want {
				t.Errorf("classifyKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// TestSessionRole_String pins the metric/log labels — these flow into
// telemetry and dashboard JSON, so a renamed label is a wire breakage.
func TestSessionRole_String(t *testing.T) {
	cases := []struct {
		role SessionRole
		want string
	}{
		{RoleIM, "im"},
		{RoleCron, "cron"},
		{RoleProject, "project"},
		{RoleScratch, "scratch"},
		{RoleSys, "sys"},
		{RolePlanner, "planner"},
		{RoleUnknown, "unknown"},
		{SessionRole(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.role.String(); got != tc.want {
			t.Errorf("SessionRole(%d).String() = %q, want %q", tc.role, got, tc.want)
		}
	}
}
