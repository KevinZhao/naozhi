package project

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// chatKeyForProbe mirrors session.chatKeyFor (router_core.go): readers
// derive a chat key by stripping the last ':'-delimited segment. It is
// duplicated here rather than imported because internal/session imports
// internal/project, so importing back would create a cycle.
func chatKeyForProbe(key string) string {
	if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
		return key[:idx]
	}
	return key
}

// TestValidateProjectName_BlocksPlannerKeyCollision pins the reason ':'
// is rejected. sessionkey.PlannerKeyFor builds `project:{name}:planner`
// and delegates validation to ValidateProjectName. Without a delimiter
// check, a directory named `foo:planner` under projects_root produces a
// planner key whose chat key is byte-identical to project `foo`'s
// planner key, colliding in the chat→sessions index and the
// workspace-override store.
//
// This test asserts BOTH halves: the collision really exists at the key
// layer (so the guard is not cargo-culted), and validation now rejects
// the input that would trigger it.
func TestValidateProjectName_BlocksPlannerKeyCollision(t *testing.T) {
	t.Parallel()

	const victim = "foo"
	const attacker = "foo:planner"

	// The collision is real at the key layer.
	collides := chatKeyForProbe(sessionkey.PlannerKeyFor(attacker)) ==
		sessionkey.PlannerKeyFor(victim)
	if !collides {
		t.Fatalf("precondition changed: chatKeyFor(PlannerKeyFor(%q))=%q no longer equals "+
			"PlannerKeyFor(%q)=%q — if the key format changed, revisit whether the ':' "+
			"guard in ValidateProjectName is still needed",
			attacker, chatKeyForProbe(sessionkey.PlannerKeyFor(attacker)),
			victim, sessionkey.PlannerKeyFor(victim))
	}

	// Validation must therefore reject the colliding name.
	if err := ValidateProjectName(attacker); err == nil {
		t.Errorf("ValidateProjectName(%q) = nil; want error — this name collides with "+
			"project %q's planner session key", attacker, victim)
	}
	// The victim name itself stays valid.
	if err := ValidateProjectName(victim); err != nil {
		t.Errorf("ValidateProjectName(%q) = %v; want nil", victim, err)
	}
}
