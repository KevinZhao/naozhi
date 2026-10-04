package session

import "testing"

func TestResolveForChat_InheritsBackendAndAccessProfile(t *testing.T) {
	ds := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:oc_x": {
				Bound:         true,
				Name:          "polyquant",
				WorkspaceDir:  "/w/polyquant",
				Backend:       "claude",
				AccessProfile: "1p-fable",
			},
		},
	}
	r := NewKeyResolver(map[string]AgentOpts{"general": {}, "coder": {}}, ds)

	t.Run("general agent inherits", func(t *testing.T) {
		_, opts := r.ResolveForChat("feishu", "group", "oc_x", "general")
		if opts.AccessProfile != "1p-fable" || opts.Backend != "claude" {
			t.Errorf("general: got backend=%q profile=%q", opts.Backend, opts.AccessProfile)
		}
	})

	t.Run("non-general agent inherits auth (correctness invariant)", func(t *testing.T) {
		_, opts := r.ResolveForChat("feishu", "group", "oc_x", "coder")
		if opts.AccessProfile != "1p-fable" || opts.Backend != "claude" {
			t.Errorf("coder: got backend=%q profile=%q, both must inherit", opts.Backend, opts.AccessProfile)
		}
	})
}

func TestAccessProfileForKey(t *testing.T) {
	ds := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:user:bob": {Bound: true, Name: "poc-jd", WorkspaceDir: "/w", AccessProfile: "bedrock-opus"},
		},
		byName: map[string]ProjectBinding{
			"poc-jd": {Bound: true, Name: "poc-jd", WorkspaceDir: "/w", AccessProfile: "bedrock-opus"},
		},
	}
	r := NewKeyResolver(map[string]AgentOpts{"general": {}}, ds)

	// IM 4-segment key for a bound non-general agent still surfaces the profile
	// via the direct binding read (ResolveForKey does not re-consult binding).
	if got := r.AccessProfileForKey("feishu:user:bob:coder"); got != "bedrock-opus" {
		t.Errorf("IM key: AccessProfileForKey = %q, want bedrock-opus", got)
	}
	// Planner key surfaces the profile via ResolveForPlannerKey.
	if got := r.AccessProfileForKey("project:poc-jd:planner"); got != "bedrock-opus" {
		t.Errorf("planner key: AccessProfileForKey = %q, want bedrock-opus", got)
	}
	// Unbound chat → no profile.
	if got := r.AccessProfileForKey("feishu:user:nobody:general"); got != "" {
		t.Errorf("unbound key: AccessProfileForKey = %q, want \"\"", got)
	}
	// Reserved namespace → no profile (cron/scratch resume own path).
	if got := r.AccessProfileForKey("cron:job1"); got != "" {
		t.Errorf("cron key: AccessProfileForKey = %q, want \"\"", got)
	}
}

// agents[].access_profile (#3106) is the tier below a project pin: it reaches
// every resolver path that builds on defaults[agentID], and a project that
// pins its own profile still wins.
func TestResolveForChat_AgentAccessProfile(t *testing.T) {
	ds := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:pinned":   {Bound: true, Name: "p1", WorkspaceDir: "/w/p1", AccessProfile: "1p-fable"},
			"feishu:group:unpinned": {Bound: true, Name: "p2", WorkspaceDir: "/w/p2"},
		},
	}
	defaults := map[string]AgentOpts{"general": {AccessProfile: "company"}, "reviewer": {AccessProfile: "personal"}}
	r := NewKeyResolver(defaults, ds)

	cases := []struct {
		name, chatID, agentID, want string
	}{
		{"unbound chat gets the agent's profile", "oc_free", "reviewer", "personal"},
		{"bound project without a pin keeps the agent's profile", "unpinned", "reviewer", "personal"},
		{"bound project pin overrides the agent", "pinned", "reviewer", "1p-fable"},
		{"planner without a pin ignores general's profile", "unpinned", "general", ""},
		{"planner takes the project pin", "pinned", "general", "1p-fable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, opts := r.ResolveForChat("feishu", "group", tc.chatID, tc.agentID)
			if opts.AccessProfile != tc.want {
				t.Errorf("ResolveForChat AccessProfile = %q, want %q", opts.AccessProfile, tc.want)
			}
		})
	}

	if opts, ok := r.ResolveForKey("dashboard:direct:1700000000-x:reviewer"); !ok || opts.AccessProfile != "personal" {
		t.Errorf("ResolveForKey(dashboard key) = %+v ok=%v, want the agent's profile", opts, ok)
	}
	if defaults["reviewer"].AccessProfile != "personal" {
		t.Error("resolver mutated its defaults map")
	}
}

// The remote-dispatch gate must see every profile a key's session can spawn
// on; an agent-level one is as host-local as a project pin.
func TestAccessProfileForKey_AgentTier(t *testing.T) {
	ds := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:pinned":   {Bound: true, Name: "p1", AccessProfile: "1p-fable"},
			"feishu:group:unpinned": {Bound: true, Name: "p2"},
		},
		byName: map[string]ProjectBinding{
			"p1": {Bound: true, Name: "p1", AccessProfile: "1p-fable"},
			"p2": {Bound: true, Name: "p2"},
		},
	}
	defaults := map[string]AgentOpts{"general": {AccessProfile: "company"}, "reviewer": {AccessProfile: "personal"}, "coder": {}}

	cases := []struct {
		name, key, want string
	}{
		{"unbound IM key", "feishu:user:bob:reviewer", "personal"},
		{"dashboard key", "dashboard:direct:1700000000-x:reviewer", "personal"},
		{"bound project without a pin", "feishu:group:unpinned:reviewer", "personal"},
		{"bound project pin wins", "feishu:group:pinned:reviewer", "1p-fable"},
		{"agent without a profile", "feishu:user:bob:coder", ""},
		{"planner without a pin", "project:p2:planner", ""},
		{"planner pin wins", "project:p1:planner", "1p-fable"},
		{"planner of an unknown project", "project:gone:planner", ""},
		{"reserved namespace", "cron:job1:x:reviewer", ""},
		{"malformed", "reviewer", ""},
	}
	r := NewKeyResolver(defaults, ds)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.AccessProfileForKey(tc.key); got != tc.want {
				t.Errorf("AccessProfileForKey(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}

	t.Run("no project data source", func(t *testing.T) {
		r := NewKeyResolver(defaults, nil)
		if got := r.AccessProfileForKey("feishu:user:bob:reviewer"); got != "personal" {
			t.Errorf("AccessProfileForKey = %q, want personal without a data source", got)
		}
	})
}

// A planner key must land on one account however it is spawned: IM chat
// (ResolveForChat), admin restart (ResolveForPlannerKey), dashboard resume
// (ResolveForKey), and the remote gate must report that same account.
func TestPlannerAccessProfile_SameOnEveryPath(t *testing.T) {
	ds := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:pinned":   {Bound: true, Name: "p1", WorkspaceDir: "/w/p1", AccessProfile: "1p-fable"},
			"feishu:group:unpinned": {Bound: true, Name: "p2", WorkspaceDir: "/w/p2"},
		},
		byName: map[string]ProjectBinding{
			"p1": {Bound: true, Name: "p1", WorkspaceDir: "/w/p1", AccessProfile: "1p-fable"},
			"p2": {Bound: true, Name: "p2", WorkspaceDir: "/w/p2"},
		},
	}
	r := NewKeyResolver(map[string]AgentOpts{"general": {AccessProfile: "company"}}, ds)

	for _, tc := range []struct{ chatID, want string }{{"pinned", "1p-fable"}, {"unpinned", ""}} {
		t.Run(tc.chatID, func(t *testing.T) {
			key, chat := r.ResolveForChat("feishu", "group", tc.chatID, "general")
			if !isPlannerKey(key) {
				t.Fatalf("ResolveForChat key = %q, want a planner key", key)
			}
			_, restart, ok := r.ResolveForPlannerKey(plannerNameFromKey(key))
			if !ok {
				t.Fatalf("ResolveForPlannerKey(%q) not found", plannerNameFromKey(key))
			}
			resume, ok := r.ResolveForKey(key)
			if !ok {
				t.Fatalf("ResolveForKey(%q) not found", key)
			}
			got := map[string]string{
				"ResolveForChat":       chat.AccessProfile,
				"ResolveForPlannerKey": restart.AccessProfile,
				"ResolveForKey":        resume.AccessProfile,
				"AccessProfileForKey":  r.AccessProfileForKey(key),
			}
			for path, ap := range got {
				if ap != tc.want {
					t.Errorf("%s AccessProfile = %q, want %q", path, ap, tc.want)
				}
			}
		})
	}
}
