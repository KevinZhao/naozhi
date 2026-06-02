package sessionkey

import "testing"

func TestPrefixConstants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"CronKeyPrefix", CronKeyPrefix, "cron:"},
		{"SysKeyPrefix", SysKeyPrefix, "sys:"},
		{"ScratchKeyPrefix", ScratchKeyPrefix, "scratch:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestKeyConstructors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"CronKey/typical", CronKey("abc123"), "cron:abc123"},
		{"CronKey/empty id", CronKey(""), "cron:"},
		{"SysKey/typical", SysKey("auto_titler"), "sys:auto_titler"},
		{"SysKey/empty id", SysKey(""), "sys:"},
		{"ScratchKey/typical", ScratchKey("s_42"), "scratch:s_42"},
		{"ScratchKey/empty id", ScratchKey(""), "scratch:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestIsCronKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"cron:abc", true},
		{"cron:", true},
		{"cron:foo:bar", true}, // longer keys still match
		{"sys:abc", false},
		{"scratch:abc", false},
		{"feishu:group:t:a", false},
		{"", false},
		{"cron", false}, // missing colon
	}
	for _, tc := range cases {
		if got := IsCronKey(tc.in); got != tc.want {
			t.Errorf("IsCronKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsSysKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"sys:auto_titler", true},
		{"sys:", true},
		{"cron:auto_titler", false},
		{"scratch:auto_titler", false},
		{"", false},
		{"sys", false},
	}
	for _, tc := range cases {
		if got := IsSysKey(tc.in); got != tc.want {
			t.Errorf("IsSysKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsScratchKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"scratch:s_42", true},
		{"scratch:", true},
		{"cron:s_42", false},
		{"sys:s_42", false},
		{"", false},
		{"scratch", false},
	}
	for _, tc := range cases {
		if got := IsScratchKey(tc.in); got != tc.want {
			t.Errorf("IsScratchKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCronJobIDFromKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"cron:abc123", "abc123"},
		{"cron:", ""},                       // valid cron key with empty id → ""
		{"sys:abc123", ""},                  // wrong namespace → ""
		{"scratch:abc123", ""},              // wrong namespace → ""
		{"", ""},                            // empty input → ""
		{"abc123", ""},                      // missing prefix → ""
		{"cron:foo:bar:baz", "foo:bar:baz"}, // colons in id preserved
	}
	for _, tc := range cases {
		if got := CronJobIDFromKey(tc.in); got != tc.want {
			t.Errorf("CronJobIDFromKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestKeyConstructorRoundTrip confirms IsCronKey + CronJobIDFromKey are
// the inverse of CronKey.
func TestKeyConstructorRoundTrip(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"abc", "16-char-hex-here", "", "with:colon"} {
		key := CronKey(id)
		if !IsCronKey(key) {
			t.Errorf("CronKey(%q) = %q, IsCronKey returned false", id, key)
		}
		if got := CronJobIDFromKey(key); got != id {
			t.Errorf("round-trip CronKey/CronJobIDFromKey: id %q, got %q", id, got)
		}
	}
}

// TestIsDashboardProjectKey covers the project-stable dashboard key
// discriminator: only `dashboard:pj:<id>:...` matches; planner / scratch /
// cron / sys / malformed inputs do not.
func TestIsDashboardProjectKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"dashboard:pj:abc123:general", true},
		{"dashboard:pj:deadbeefdeadbeef:sonnet", true},
		{"dashboard:pj:x", true},          // minimal non-empty id, no agent segment
		{"dashboard:pj:", false},          // empty id segment
		{"dashboard:pj", false},           // missing trailing colon
		{"dashboard:direct:ts-slug:general", false}, // legacy timestamp key
		{"project:foo:planner", false},    // planner namespace (platform=project)
		{"dashboard:project:abc:general", false}, // chatType "project" is NOT "pj"
		{"scratch:abc:general:sonnet", false},
		{"cron:abc123", false},
		{"sys:daemon", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsDashboardProjectKey(tc.in); got != tc.want {
			t.Errorf("IsDashboardProjectKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDashboardProjectKeyVsPlannerNoCollision pins the BLOCKING-1 fix: the
// project-stable dashboard namespace and the planner namespace must never
// classify the same key. Includes the specific reviewer counterexample —
// a naive parts[1]=="project" check would misfire, but "pj" shares no token.
func TestDashboardProjectKeyVsPlannerNoCollision(t *testing.T) {
	t.Parallel()
	dashKey := DashboardPlatform + ":" + DashboardProjectChatType + ":hash16:general"
	if !IsDashboardProjectKey(dashKey) {
		t.Fatalf("expected %q to be a dashboard project key", dashKey)
	}
	if IsPlannerKey(dashKey) {
		t.Errorf("dashboard project key %q must not be classified as planner", dashKey)
	}
	plannerKey := PlannerKeyFor("foo")
	if IsDashboardProjectKey(plannerKey) {
		t.Errorf("planner key %q must not be classified as dashboard project", plannerKey)
	}
	if !IsPlannerKey(plannerKey) {
		t.Errorf("planner key %q should be a planner key", plannerKey)
	}
}
