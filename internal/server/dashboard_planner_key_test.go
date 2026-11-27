package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// TestBuildSessionOpts_PlannerKey: a planner key resolves its project's
// workspace and is exempt, including for a directory name containing ':'
// (R20260531-QUAL-4: the old SplitN extraction truncated "my:proj" to "my").
func TestBuildSessionOpts_PlannerKey(t *testing.T) {
	root := t.TempDir()
	names := []string{"webapp", "my:proj"}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, n := range names {
		// resolver=nil: buildSessionOpts resolves through one over mgr.
		opts := buildSessionOpts(project.PlannerKeyFor(n), nil, map[string]session.AgentOpts{"general": {}}, mgr)
		if !opts.Exempt {
			t.Errorf("%s: planner opts.Exempt = false, want true", n)
		}
		if want := filepath.Join(root, n); opts.Workspace != want {
			t.Errorf("%s: planner Workspace = %q, want %q", n, opts.Workspace, want)
		}
	}
}

// Only an IM agent key spawns on an agent's profile and backend, as
// session.KeyResolver decides it. A planner's account is its project's pin
// only; cron:, scratch: and malformed keys get neither, so the session never
// records general's.
func TestBuildSessionOpts_NonAgentKeysIgnoreGeneralAgentTier(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"unpinned", "pinned"} {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := mgr.UpdateConfig("pinned", project.ProjectConfig{AccessProfile: "personal"}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	agents := map[string]session.AgentOpts{"general": {AccessProfile: "company", DefaultBackend: "kiro"}}
	// A resolver with no project data misses every planner key (ok=false).
	missing := session.NewKeyResolver(agents, nil)

	cases := []struct {
		name     string
		key      string
		resolver *session.KeyResolver
		want     string
	}{
		{"project gone, resolver miss", project.PlannerKeyFor("gone"), missing, ""},
		{"project gone, no resolver", project.PlannerKeyFor("gone"), nil, ""},
		{"project without a pin", project.PlannerKeyFor("unpinned"), nil, ""},
		{"project pin", project.PlannerKeyFor("pinned"), nil, "personal"},
		{"chat key keeps the agent profile", "feishu:direct:alice:general", nil, "company"},
		{"cron key, resolver miss", "cron:job1", missing, ""},
		{"cron key, no resolver", "cron:job1", nil, ""},
		{"scratch key, resolver miss", "scratch:abc", missing, ""},
		{"four-segment cron key", "cron:job1:x:general", nil, ""},
		{"malformed key", "feishu:direct", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := buildSessionOpts(tc.key, tc.resolver, agents, mgr)
			if opts.AccessProfile != tc.want {
				t.Errorf("AccessProfile = %q, want %q", opts.AccessProfile, tc.want)
			}
			wantBackend := ""
			if tc.key == "feishu:direct:alice:general" {
				wantBackend = "kiro"
			}
			if opts.DefaultBackend != wantBackend {
				t.Errorf("DefaultBackend = %q, want %q", opts.DefaultBackend, wantBackend)
			}
		})
	}
	if agents["general"].AccessProfile != "company" {
		t.Errorf("agents map mutated: %+v", agents["general"])
	}
}

// A planner key gets the resolver's project-only opts from buildSessionOpts,
// with or without a resolver: the project's backend pin applies and general's
// model, prompt, args and effort do not. A planner whose project is gone gets
// blank exempt opts rather than general's.
func TestBuildSessionOpts_PlannerMatchesResolver(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "kp"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := mgr.UpdateConfig("kp", project.ProjectConfig{Backend: "kiro", PlannerPrompt: "P"}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	agents := map[string]session.AgentOpts{"general": {
		Model: "sonnet", SystemPrompt: "G", ExtraArgs: []string{"--x"}, Effort: "high",
	}}
	resolver := session.NewKeyResolver(agents, project.NewDataSource(mgr))
	key := project.PlannerKeyFor("kp")

	want, ok := resolver.ResolveForKey(key)
	if !ok {
		t.Fatalf("precondition: resolver misses %q", key)
	}
	if want.Backend != "kiro" || want.SystemPrompt != "P" || want.Model != "" {
		t.Fatalf("precondition: resolver opts = %#v", want)
	}
	for name, r := range map[string]*session.KeyResolver{"resolver": resolver, "no resolver": nil} {
		if got := buildSessionOpts(key, r, agents, mgr); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: buildSessionOpts = %#v, want the resolver's %#v", name, got, want)
		}
	}

	gone := project.PlannerKeyFor("gone")
	for name, r := range map[string]*session.KeyResolver{"resolver": resolver, "no resolver": nil} {
		if got := buildSessionOpts(gone, r, agents, mgr); !reflect.DeepEqual(got, session.AgentOpts{Exempt: true}) {
			t.Errorf("%s: deleted project's planner = %#v, want blank exempt opts", name, got)
		}
	}
}
