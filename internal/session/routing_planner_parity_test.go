package session

import (
	"reflect"
	"testing"
)

// A planner key must spawn with the same opts however it is started: IM chat
// (ResolveForChat), admin restart (ResolveForPlannerKey) and dashboard resume
// (ResolveForKey). Only the project's values count; nothing of
// defaults["general"] reaches the planner, whether or not the project sets a
// planner model and prompt.
func TestPlannerOpts_SameOnEveryPath(t *testing.T) {
	t.Parallel()
	general := AgentOpts{
		Model:          "sonnet",
		Effort:         "high",
		ExtraArgs:      []string{"--x"},
		SystemPrompt:   "G",
		AccessProfile:  "company",
		DefaultBackend: "claude",
	}
	cases := []struct {
		name    string
		binding ProjectBinding
		want    AgentOpts
	}{
		{
			name: "planner_model_and_prompt_set",
			binding: ProjectBinding{
				Bound: true, Name: "p", WorkspaceDir: "/w/p",
				PlannerModel: "opus", PlannerPrompt: "P",
				Backend: "kiro", AccessProfile: "1p",
			},
			want: AgentOpts{
				Exempt: true, Workspace: "/w/p", Model: "opus", SystemPrompt: "P",
				Backend: "kiro", AccessProfile: "1p",
			},
		},
		{
			name: "planner_model_and_prompt_empty",
			binding: ProjectBinding{
				Bound: true, Name: "p", WorkspaceDir: "/w/p",
				Backend: "kiro", AccessProfile: "1p",
			},
			want: AgentOpts{
				Exempt: true, Workspace: "/w/p", Backend: "kiro", AccessProfile: "1p",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := &fakeDataSource{
				byChat: map[string]ProjectBinding{"feishu:group:c1": tc.binding},
				byName: map[string]ProjectBinding{"p": tc.binding},
			}
			defaults := map[string]AgentOpts{"general": general}
			r := NewKeyResolver(defaults, ds)

			key, chat := r.ResolveForChat("feishu", "group", "c1", "general")
			restartKey, restart, ok := r.ResolveForPlannerKey("p")
			if !ok {
				t.Fatal("ResolveForPlannerKey(p) not found")
			}
			resume, ok := r.ResolveForKey(key)
			if !ok {
				t.Fatalf("ResolveForKey(%q) not found", key)
			}
			if key != restartKey {
				t.Errorf("ResolveForChat key %q != ResolveForPlannerKey key %q", key, restartKey)
			}
			for path, got := range map[string]AgentOpts{
				"ResolveForChat":       chat,
				"ResolveForPlannerKey": restart,
				"ResolveForKey":        resume,
			} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s opts:\n got: %#v\nwant: %#v", path, got, tc.want)
				}
			}
			if !reflect.DeepEqual(defaults["general"], general) {
				t.Errorf("defaults[general] mutated: %#v", defaults["general"])
			}
		})
	}
}
