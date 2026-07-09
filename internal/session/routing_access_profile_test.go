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
