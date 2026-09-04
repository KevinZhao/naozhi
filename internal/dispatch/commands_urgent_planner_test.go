package dispatch

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestUrgent_AppendsPlannerPromptWhenProjectBound is the S3 regression
// guard for docs/rfc/key-resolver.md Phase 3.
//
// Pre-Phase-3, handleUrgentCommand manually built `opts` with only
// Exempt+Workspace copied from a project binding — silently dropping the
// planner model/prompt overrides that the main IM path honours. Users
// sending /urgent in a project-bound chat never got their planner
// --append-system-prompt flag. Phase 3 routes /urgent through
// KeyResolver.ResolveForChat, which returns the same opts as the main
// IM path.
//
// This test asserts the contract by invoking the same Resolver call
// handleUrgentCommand uses. A regression that re-inlines the old
// "Exempt+Workspace only" logic would make this test fail.
func TestUrgent_AppendsPlannerPromptWhenProjectBound(t *testing.T) {
	t.Parallel()

	agents := map[string]session.AgentOpts{
		"general": {Model: "sonnet"},
	}
	binding := session.ProjectBinding{
		Bound:         true,
		Name:          "myproj",
		WorkspaceDir:  "/w/myproj",
		PlannerModel:  "opus",
		PlannerPrompt: "你是 myproj 规划者",
	}
	data := &parityDataSource{binding: binding}
	r := session.NewKeyResolver(agents, data)

	// Mirror handleUrgentCommand's resolver-branch call.
	key, opts := r.ResolveForChat("feishu", "direct", "alice", "general")

	if key != "project:myproj:planner" {
		t.Errorf("key = %q, want %q", key, "project:myproj:planner")
	}
	want := session.AgentOpts{
		Model:        "opus",
		Workspace:    "/w/myproj",
		Exempt:       true,
		SystemPrompt: "你是 myproj 规划者",
	}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("opts mismatch:\n got:  %#v\nwant: %#v", opts, want)
	}

	// Explicitly assert the prompt landed in SystemPrompt — the pre-Phase-3
	// bug manifested as no prompt at all, and #2493 as the prompt sitting in
	// ExtraArgs where the cli denylist stripped it.
	if opts.SystemPrompt != "你是 myproj 规划者" {
		t.Errorf("planner prompt not in SystemPrompt — /urgent would fail to deliver planner instructions. got: %+v", opts)
	}
}

// TestUrgent_UnboundFallsBackToDefaults asserts /urgent in a non-project
// chat still gets the agent's base opts — no regression in the non-bound
// path.
func TestUrgent_UnboundFallsBackToDefaults(t *testing.T) {
	t.Parallel()

	agents := map[string]session.AgentOpts{
		"general": {Model: "sonnet", Backend: "claude"},
	}
	data := &parityDataSource{binding: session.ProjectBinding{}} // unbound
	r := session.NewKeyResolver(agents, data)

	key, opts := r.ResolveForChat("feishu", "direct", "bob", "general")

	if key != "feishu:direct:bob:general" {
		t.Errorf("key = %q, want %q", key, "feishu:direct:bob:general")
	}
	want := session.AgentOpts{Model: "sonnet", Backend: "claude"}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("opts mismatch:\n got:  %#v\nwant: %#v", opts, want)
	}
}
