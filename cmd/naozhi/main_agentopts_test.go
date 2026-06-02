package main

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/session"
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
			"planner": {Model: "opus"},
		},
	}
	agents, cronAgents := buildAgentOpts(cfg)

	if len(agents) != 2 || len(cronAgents) != 2 {
		t.Fatalf("len(agents)=%d len(cronAgents)=%d, want 2/2", len(agents), len(cronAgents))
	}
	if got := agents["general"]; got.Model != "sonnet" || len(got.ExtraArgs) != 1 || got.ExtraArgs[0] != "--foo" {
		t.Errorf("agents[general] = %+v, want model=sonnet args=[--foo]", got)
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

// TestFirstUndefinedAgentCommand verifies the agent_commands cross-reference
// check extracted from main() (#590): all-resolve returns ok=true with an
// empty command; a dangling reference returns ok=false naming the command.
func TestFirstUndefinedAgentCommand(t *testing.T) {
	t.Parallel()

	agents := map[string]session.AgentOpts{"general": {}, "planner": {}}

	// All commands resolve.
	cmd, ok := firstUndefinedAgentCommand(map[string]string{
		"/ask":  "general",
		"/plan": "planner",
	}, agents)
	if !ok || cmd != "" {
		t.Fatalf("all-resolve: got (%q, %v), want (\"\", true)", cmd, ok)
	}

	// Nil / empty command map resolves trivially.
	if cmd, ok := firstUndefinedAgentCommand(nil, agents); !ok || cmd != "" {
		t.Fatalf("nil commands: got (%q, %v), want (\"\", true)", cmd, ok)
	}

	// Dangling reference surfaces the offending command.
	cmd, ok = firstUndefinedAgentCommand(map[string]string{"/x": "ghost"}, agents)
	if ok || cmd != "/x" {
		t.Fatalf("dangling: got (%q, %v), want (\"/x\", false)", cmd, ok)
	}
}
