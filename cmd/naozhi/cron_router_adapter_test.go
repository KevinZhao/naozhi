// cron_router_adapter_test.go pins the round-trip behaviour of
// cron.AgentOpts ↔ session.AgentOpts and the cron.InterruptOutcome
// ordinals against session.InterruptOutcome. Without these tests the
// init() panic in cron_router_adapter.go is the only protection
// against ordinal drift, and it only fires at boot — silent miscasts
// in CI / sandbox builds slip through.
//
// Refs: docs/rfc/cron-sysession-merge.md Phase B (§3.3.3).

package main

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// TestToSessionAgentOpts_ExtraArgsCloned verifies that mutating the
// cron-side ExtraArgs after toSessionAgentOpts returns does NOT corrupt
// the session-side slice. The aliasing contract in
// internal/session/router_lifecycle.go:267 says callers populating
// AgentOpts must own ExtraArgs exclusively; the adapter clones to
// honour that.
func TestToSessionAgentOpts_ExtraArgsCloned(t *testing.T) {
	t.Parallel()
	cronArgs := []string{"--debug", "--verbose"}
	in := cron.AgentOpts{
		Backend:   "claude",
		Model:     "opus",
		Workspace: "/tmp/x",
		ExtraArgs: cronArgs,
		Exempt:    true,
	}
	out := toSessionAgentOpts(in)
	if len(out.ExtraArgs) != 2 || out.ExtraArgs[0] != "--debug" || out.ExtraArgs[1] != "--verbose" {
		t.Fatalf("ExtraArgs not copied: got %#v", out.ExtraArgs)
	}
	// Mutate the cron source — session copy must stay unchanged.
	cronArgs[0] = "--mutated"
	if out.ExtraArgs[0] != "--debug" {
		t.Errorf("ExtraArgs aliased: out[0] = %q after mutating cron source, want %q",
			out.ExtraArgs[0], "--debug")
	}
}

// TestToCronAgentOpts_ExtraArgsCloned mirrors the test above for the
// reverse direction, used at boot to translate cfg.Agents
// (session.AgentOpts) → cron.AgentOpts in the cron Scheduler's agents
// map.
func TestToCronAgentOpts_ExtraArgsCloned(t *testing.T) {
	t.Parallel()
	sessArgs := []string{"--a", "--b"}
	in := session.AgentOpts{
		Backend:   "kiro",
		Model:     "sonnet",
		Workspace: "/var/y",
		ExtraArgs: sessArgs,
		Exempt:    false,
	}
	out := toCronAgentOpts(in)
	if len(out.ExtraArgs) != 2 || out.ExtraArgs[0] != "--a" || out.ExtraArgs[1] != "--b" {
		t.Fatalf("ExtraArgs not copied: got %#v", out.ExtraArgs)
	}
	sessArgs[1] = "--mutated"
	if out.ExtraArgs[1] != "--b" {
		t.Errorf("ExtraArgs aliased: out[1] = %q after mutating session source, want %q",
			out.ExtraArgs[1], "--b")
	}
}

// TestToSessionAgentOpts_NilExtraArgs ensures a nil/empty cron
// ExtraArgs translates to a nil session ExtraArgs, not an empty
// non-nil slice that could surprise downstream nil checks.
func TestToSessionAgentOpts_NilExtraArgs(t *testing.T) {
	t.Parallel()
	out := toSessionAgentOpts(cron.AgentOpts{Backend: "claude"})
	if out.ExtraArgs != nil {
		t.Errorf("empty ExtraArgs: got %#v, want nil", out.ExtraArgs)
	}
}

// TestInterruptOutcome_Ordinals duplicates the init() panic check at
// test time so a divergence is caught by `go test` in CI even before
// the binary is booted. Without this, a refactor that reorders
// session.InterruptOutcome would only fail at first boot of the
// naozhi binary, which CI does not execute.
func TestInterruptOutcome_Ordinals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    int
		s    int
	}{
		{"Sent", int(cron.InterruptSent), int(session.InterruptSent)},
		{"NoSession", int(cron.InterruptNoSession), int(session.InterruptNoSession)},
		{"NoTurn", int(cron.InterruptNoTurn), int(session.InterruptNoTurn)},
		{"Unsupported", int(cron.InterruptUnsupported), int(session.InterruptUnsupported)},
		{"Error", int(cron.InterruptError), int(session.InterruptError)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c != tc.s {
				t.Errorf("%s ordinal diverged: cron=%d, session=%d", tc.name, tc.c, tc.s)
			}
		})
	}
}

// TestSessionStatus_Cast verifies cron.SessionStatus(int(...)) round-
// trip preserves the three known states. cron does not branch on
// SessionStatus today, but a future caller comparing against
// SessionExisting / SessionResumed / SessionNew relies on the cast
// staying identity.
func TestSessionStatus_Cast(t *testing.T) {
	t.Parallel()
	if int(cron.SessionExisting) != int(session.SessionExisting) {
		t.Errorf("SessionExisting ordinal: cron=%d, session=%d",
			cron.SessionExisting, session.SessionExisting)
	}
	if int(cron.SessionResumed) != int(session.SessionResumed) {
		t.Errorf("SessionResumed ordinal: cron=%d, session=%d",
			cron.SessionResumed, session.SessionResumed)
	}
	if int(cron.SessionNew) != int(session.SessionNew) {
		t.Errorf("SessionNew ordinal: cron=%d, session=%d",
			cron.SessionNew, session.SessionNew)
	}
}
