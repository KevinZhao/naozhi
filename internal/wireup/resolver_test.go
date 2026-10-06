package wireup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// A cron key resolves to the profile of the agent its job's prompt routes to;
// an unrouted prompt, an unknown job and a nil scheduler all give "".
func TestKeyResolver_CronAccessProfile(t *testing.T) {
	t.Parallel()
	agentCommands := map[string]string{"review": "reviewer"}
	agents := map[string]session.AgentOpts{"general": {}, "reviewer": {AccessProfile: "personal"}}
	sched := cron.NewScheduler(cron.SchedulerConfig{MaxJobs: 4, AllowNilRouter: true}, cron.SchedulerDeps{
		Agents:        map[string]cron.AgentOpts{"general": {}, "reviewer": {AccessProfile: "personal"}},
		AgentCommands: agentCommands,
	})
	pinned := &cron.Job{Schedule: "@every 30m", Prompt: "/review the diff", Paused: true}
	plain := &cron.Job{Schedule: "@every 30m", Prompt: "hello", Paused: true}
	for _, j := range []*cron.Job{pinned, plain} {
		if err := sched.AddJob(j); err != nil { // assigns j.ID
			t.Fatalf("AddJob: %v", err)
		}
	}

	r := KeyResolver(agents, agentCommands, nil, sched)
	cases := map[string]string{
		sessionkey.CronKey(pinned.ID): "personal",
		sessionkey.CronKey(plain.ID):  "",
		sessionkey.CronKey("missing"): "",
		"feishu:user:bob:reviewer":    "personal",
	}
	for key, want := range cases {
		if got := r.AccessProfileForKey(key); got != want {
			t.Errorf("AccessProfileForKey(%q) = %q, want %q", key, got, want)
		}
	}

	noSched := KeyResolver(agents, agentCommands, nil, nil)
	if got := noSched.AccessProfileForKey(sessionkey.CronKey(pinned.ID)); got != "" {
		t.Errorf("nil scheduler: cron key profile = %q, want \"\"", got)
	}
	if got := noSched.AccessProfileForKey("feishu:user:bob:reviewer"); got != "personal" {
		t.Errorf("nil scheduler: agent key profile = %q, want personal", got)
	}
}

// The projects argument reaches the resolver: a planner key resolves only for
// a project the Manager knows.
func TestKeyResolver_ProjectsReachPlannerView(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, opts, ok := KeyResolver(nil, nil, mgr, nil).ResolveForPlannerKey("alpha"); !ok || opts.Model != "opus" {
		t.Errorf("ResolveForPlannerKey(alpha) = %+v, %v; want the project's planner opts", opts, ok)
	}
	if _, _, ok := KeyResolver(nil, nil, nil, nil).ResolveForPlannerKey("alpha"); ok {
		t.Error("nil projects: ResolveForPlannerKey(alpha) ok = true, want false")
	}
}
