package dispatch

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestDispatchResolverParity asserts the main IM path's Resolver branch
// and legacy inlined-merge branch produce byte-identical (key, opts) for
// every combination tested. The main IM path in BuildHandler uses
// `d.resolver != nil` to switch between them. During Phase 2 both
// branches coexist; this test is the regression guard that they stay in
// lockstep until legacy is removed.
//
// Matrix covers: agentID general/non-general × project
// bound/unbound × PlannerModel empty/set × PlannerPrompt empty/set ×
// defaults.ExtraArgs nil/empty/shared-cap.
func TestDispatchResolverParity(t *testing.T) {
	t.Parallel()

	makeShared := func(n int) []string {
		// cap > len so two-arg append would write past len
		s := make([]string, 1, n)
		s[0] = "--existing"
		return s
	}

	type setup struct {
		name    string
		agents  map[string]session.AgentOpts
		binding session.ProjectBinding
		agentID string
	}
	fixtures := []setup{
		{
			name: "unbound_general_empty_defaults",
			agents: map[string]session.AgentOpts{
				"general": {Model: "sonnet"},
			},
			agentID: "general",
		},
		{
			name: "unbound_nongeneral",
			agents: map[string]session.AgentOpts{
				"code-reviewer": {Model: "opus", ExtraArgs: []string{"--verbose"}},
			},
			agentID: "code-reviewer",
		},
		{
			name: "bound_general_no_planner_override",
			agents: map[string]session.AgentOpts{
				"general": {Model: "sonnet"},
			},
			binding: session.ProjectBinding{
				Bound: true, Name: "myproj", WorkspaceDir: "/w/myproj",
			},
			agentID: "general",
		},
		{
			name: "bound_general_full_planner_override",
			agents: map[string]session.AgentOpts{
				"general": {Model: "sonnet", Backend: "claude", ExtraArgs: makeShared(8)},
			},
			binding: session.ProjectBinding{
				Bound: true, Name: "myproj", WorkspaceDir: "/w/myproj",
				PlannerModel: "opus", PlannerPrompt: "P",
			},
			agentID: "general",
		},
		{
			name: "bound_nongeneral_workspace_only",
			agents: map[string]session.AgentOpts{
				"code-reviewer": {Model: "opus"},
			},
			binding: session.ProjectBinding{
				Bound: true, Name: "myproj", WorkspaceDir: "/w/myproj",
				// planner fields must be ignored for non-general
				PlannerModel: "IGNORED", PlannerPrompt: "IGNORED",
			},
			agentID: "code-reviewer",
		},
		{
			name: "unknown_agent_zero_defaults",
			agents: map[string]session.AgentOpts{
				"general": {Model: "sonnet"},
			},
			agentID: "nonexistent",
		},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			// Each branch gets an independent defaults-map copy so
			// aliasing canary assertions don't leak between branches.
			agentsLegacy := cloneAgents(f.agents)
			agentsResolver := cloneAgents(f.agents)

			// Build resolver branch inputs.
			data := &parityDataSource{binding: f.binding}
			r := session.NewKeyResolver(agentsResolver, data)

			// --- Resolver branch ---
			const platform, chatType, chatID = "feishu", "direct", "alice"
			resolverKey, resolverOpts := r.ResolveForChat(platform, chatType, chatID, f.agentID)

			// --- Legacy branch (manual replica of dispatch.go) ---
			legacyKey, legacyOpts := legacyMerge(agentsLegacy, f.binding, f.agentID, platform, chatType, chatID)

			if resolverKey != legacyKey {
				t.Errorf("key mismatch:\n resolver: %q\n legacy:   %q", resolverKey, legacyKey)
			}

			// Compare structurally; ExtraArgs nil vs empty slice differ in
			// reflect.DeepEqual and both branches must agree on the shape.
			if !reflect.DeepEqual(resolverOpts, legacyOpts) {
				t.Errorf("opts mismatch:\n resolver: %#v\n legacy:   %#v", resolverOpts, legacyOpts)
			}

			// Shared-slice aliasing parity: if defaults had cap > len,
			// both branches must leave the original backing array
			// untouched past len.
			assertNoAliasingLeak(t, "resolver", agentsResolver, f.agentID)
			assertNoAliasingLeak(t, "legacy", agentsLegacy, f.agentID)
		})
	}
}

// legacyMerge is the pre-Phase-2 inlined merge logic. Kept here
// verbatim from dispatch.go legacy branch so parity test can call it
// in isolation. Update both sides together or this test goes stale.
func legacyMerge(agents map[string]session.AgentOpts, binding session.ProjectBinding, agentID, platform, chatType, chatID string) (string, session.AgentOpts) {
	var key string
	opts := agents[agentID]
	if binding.Bound {
		if agentID == "general" {
			key = "project:" + binding.Name + ":planner"
			opts.Exempt = true
			opts.Workspace = binding.WorkspaceDir
			if binding.PlannerModel != "" {
				opts.Model = binding.PlannerModel
			}
			if binding.PlannerPrompt != "" {
				opts.ExtraArgs = append(
					opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)],
					"--append-system-prompt", binding.PlannerPrompt,
				)
			}
		} else {
			key = session.SessionKey(platform, chatType, chatID, agentID)
			opts.Workspace = binding.WorkspaceDir
		}
	}
	if key == "" {
		key = session.SessionKey(platform, chatType, chatID, agentID)
	}
	return key, opts
}

func cloneAgents(src map[string]session.AgentOpts) map[string]session.AgentOpts {
	dst := make(map[string]session.AgentOpts, len(src))
	for k, v := range src {
		cp := v
		// Preserve cap so aliasing canary still fires. A naive
		// append(nil, v.ExtraArgs...) would collapse cap to len.
		if v.ExtraArgs != nil {
			extra := make([]string, len(v.ExtraArgs), cap(v.ExtraArgs))
			copy(extra, v.ExtraArgs)
			cp.ExtraArgs = extra
		}
		dst[k] = cp
	}
	return dst
}

func assertNoAliasingLeak(t *testing.T, label string, agents map[string]session.AgentOpts, agentID string) {
	t.Helper()
	orig, ok := agents[agentID]
	if !ok || orig.ExtraArgs == nil {
		return
	}
	peek := orig.ExtraArgs[:cap(orig.ExtraArgs)]
	for i := len(orig.ExtraArgs); i < cap(orig.ExtraArgs); i++ {
		if peek[i] != "" {
			t.Errorf("%s branch: aliasing leak in defaults[%q].ExtraArgs at index %d: %q",
				label, agentID, i, peek[i])
		}
	}
}

// parityDataSource serves a single fixed ProjectBinding for
// ResolveForChat. Doesn't implement ProjectByName (not exercised here).
type parityDataSource struct {
	binding session.ProjectBinding
}

func (d *parityDataSource) ProjectBinding(platform, chatType, chatID string) session.ProjectBinding {
	return d.binding
}

func (d *parityDataSource) ProjectByName(name string) (session.ProjectBinding, bool) {
	if d.binding.Bound && d.binding.Name == name {
		return d.binding, true
	}
	return session.ProjectBinding{}, false
}
