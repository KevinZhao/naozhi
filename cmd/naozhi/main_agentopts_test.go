package main

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

// TestBuildAgentOpts covers the cfg.Agents → session/cron map translation
// extracted from main() in R237-ARCH-8 (#590): fields copy through, the
// cron view is the toCronAgentOpts projection, and both maps are non-nil
// even for an empty config.
func TestBuildAgentOpts(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"general": {Model: "sonnet", Args: []string{"--foo"}},
			// Effort here also exercises the cron round-trip below via the
			// DeepEqual projection check: dropping it from either adapter
			// direction breaks that comparison.
			"planner": {Model: "opus", Effort: "max"},
			// #2493: agents[].system_prompt must survive both hops too.
			"reviewer": {Model: "sonnet", SystemPrompt: "You are a code review expert.", AccessProfile: "personal", Backend: "kiro"},
		},
	}
	agents, cronAgents := buildAgentOpts(cfg)

	// #3106: without this hop the agent's sessions spawn on the default
	// account while config check reports the pinned one.
	if got := agents["reviewer"].AccessProfile; got != "personal" {
		t.Errorf("agents[reviewer].AccessProfile = %q, want personal", got)
	}
	if got := agents["general"].AccessProfile; got != "" {
		t.Errorf("agents[general].AccessProfile = %q, want empty (unset in config)", got)
	}
	// Cron runs the agent's jobs on the same account (the cron → session half
	// lives in internal/wireup/cron_router_adapter_test.go).
	if got := cronAgents["reviewer"].AccessProfile; got != "personal" {
		t.Errorf("cronAgents[reviewer].AccessProfile = %q, want personal", got)
	}

	// agents[].backend is the agent tier, not the explicit one: as Backend it
	// would outrank the dashboard pick and resume continuity. Cron has no
	// picker, so there it becomes the job's backend.
	if got := agents["reviewer"]; got.DefaultBackend != "kiro" || got.Backend != "" {
		t.Errorf("agents[reviewer] Backend=%q DefaultBackend=%q, want \"\"/kiro", got.Backend, got.DefaultBackend)
	}
	if got := cronAgents["reviewer"].Backend; got != "kiro" {
		t.Errorf("cronAgents[reviewer].Backend = %q, want kiro", got)
	}

	if got := agents["reviewer"].SystemPrompt; got != "You are a code review expert." {
		t.Errorf("agents[reviewer].SystemPrompt = %q, want the configured prompt", got)
	}
	if got := cronAgents["reviewer"].SystemPrompt; got != "You are a code review expert." {
		t.Errorf("cronAgents[reviewer].SystemPrompt = %q, want the configured prompt", got)
	}
	delete(agents, "reviewer")
	delete(cronAgents, "reviewer")

	if len(agents) != 2 || len(cronAgents) != 2 {
		t.Fatalf("len(agents)=%d len(cronAgents)=%d, want 2/2", len(agents), len(cronAgents))
	}
	if got := agents["general"]; got.Model != "sonnet" || len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "--foo" {
		t.Errorf("agents[general] = %+v, want model=sonnet args=[--foo]", got)
	}
	// agents[].effort must survive the config → session.AgentOpts hop; without
	// it the per-agent tier silently never reaches spawn.
	if got := agents["planner"].Effort; got != "max" {
		t.Errorf("agents[planner].Effort = %q, want max", got)
	}
	if got := agents["general"].Effort; got != "" {
		t.Errorf("agents[general].Effort = %q, want empty (unset in config)", got)
	}
	// The cron projection must carry it too, so a job scheduled for that agent
	// inherits the tier. (The cron → session half lives in
	// internal/wireup/cron_router_adapter_test.go, which owns that function.)
	if got := cronAgents["planner"].Effort; got != "max" {
		t.Errorf("cronAgents[planner].Effort = %q, want max", got)
	}
	if agents["planner"].Model != "opus" {
		t.Errorf("agents[planner].Model = %q, want opus", agents["planner"].Model)
	}
	// cron view must agree with the dedicated translator for each id.
	for id, a := range agents {
		if !reflect.DeepEqual(cronAgents[id], toCronAgentOpts(a)) {
			t.Errorf("cronAgents[%q] = %+v, want toCronAgentOpts projection", id, cronAgents[id])
		}
	}

	// Empty config: non-nil empty maps (main ranges over them unconditionally).
	emptyAgents, emptyCron := buildAgentOpts(&config.Config{})
	if emptyAgents == nil || emptyCron == nil {
		t.Fatalf("buildAgentOpts(empty) returned nil map(s): %v / %v", emptyAgents, emptyCron)
	}
	if len(emptyAgents) != 0 || len(emptyCron) != 0 {
		t.Errorf("buildAgentOpts(empty) = %d/%d entries, want 0/0", len(emptyAgents), len(emptyCron))
	}
}
