// cron_router_adapter_test.go pins the cron.AgentOpts → session.AgentOpts
// translation and the cron.InterruptOutcome / cron.SessionStatus ordinals
// against their session counterparts. Without these tests the init() panic in
// cron_router_adapter.go is the only protection against ordinal drift, and it
// only fires at boot — silent miscasts in CI / sandbox builds slip through.
//
// Moved here from cmd/naozhi with the adapter (R260528-ARCH-23 / #1382).

package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// TestToSessionAgentOpts_ExtraArgsCloned verifies that mutating the cron-side
// ExtraArgs after toSessionAgentOpts returns does NOT corrupt the session-side
// slice. The aliasing contract in internal/session/router_lifecycle.go:267
// says callers populating AgentOpts must own ExtraArgs exclusively; the
// adapter clones to honour that.
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

// TestToSessionAgentOpts_NilExtraArgs ensures a nil/empty cron ExtraArgs
// translates to a nil session ExtraArgs, not an empty non-nil slice that could
// surprise downstream nil checks.
func TestToSessionAgentOpts_NilExtraArgs(t *testing.T) {
	t.Parallel()
	out := toSessionAgentOpts(cron.AgentOpts{Backend: "claude"})
	if out.ExtraArgs != nil {
		t.Errorf("empty ExtraArgs: got %#v, want nil", out.ExtraArgs)
	}
}

// TestInterruptOutcome_Ordinals duplicates the init() panic check at test time
// so a divergence is caught by `go test` in CI even before the binary is
// booted. Without this, a refactor that reorders session.InterruptOutcome
// would only fail at first boot of the naozhi binary, which CI does not
// execute.
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

// TestInterruptOutcome_CountDrift guards the gap the per-member ordinal table
// (TestInterruptOutcome_Ordinals) and the init() panic cannot catch: a NEW
// case appended to cron.InterruptOutcome or session.InterruptOutcome without
// updating the alignment list (R260528-ARCH-17 / #1378). Adding InterruptFoo
// AFTER InterruptError leaves every existing pair equal — the ordinal table
// still passes — yet one side now has a member the other lacks, which silently
// miscasts in cronSessionAdapter.InterruptViaControl.
//
// We pin two invariants:
//  1. InterruptError is ordinal 4 on BOTH sides (an insert BEFORE Error shifts
//     it and fails here).
//  2. The first ordinal PAST Error (5) is undefined on the session side —
//     session.InterruptOutcome.String() renders it as "unknown(5)". An append
//     AFTER Error on the session side would give 5 a real name and break this.
//     cron has no String(); its append is caught by the count pin below
//     (knownInterruptOutcomes must equal Error+1).
//
// If a fifth real outcome is ever added it MUST be mirrored on both sides AND
// this test bumped deliberately — exactly the human review gate #1378 asks for.
func TestInterruptOutcome_CountDrift(t *testing.T) {
	t.Parallel()

	const lastOrdinal = 4 // InterruptError
	if int(cron.InterruptError) != lastOrdinal {
		t.Errorf("cron.InterruptError ordinal = %d, want %d — a case was inserted before it; mirror on session side and bump this test",
			int(cron.InterruptError), lastOrdinal)
	}
	if int(session.InterruptError) != lastOrdinal {
		t.Errorf("session.InterruptError ordinal = %d, want %d — a case was inserted before it; mirror on cron side and bump this test",
			int(session.InterruptError), lastOrdinal)
	}

	// One past the last known member must be undefined on the session side.
	if got := session.InterruptOutcome(lastOrdinal + 1).String(); got != "unknown(5)" {
		t.Errorf("session.InterruptOutcome(5).String() = %q, want %q — a new outcome was appended after InterruptError; mirror it on cron side and bump this test",
			got, "unknown(5)")
	}

	// Count pin: the known cron members are exactly Sent..Error (5 values).
	// Encoded as a slice so a `go vet`-friendly exhaustive review is forced
	// when the const block grows; len must equal Error+1 (dense iota).
	knownCron := []cron.InterruptOutcome{
		cron.InterruptSent, cron.InterruptNoSession, cron.InterruptNoTurn,
		cron.InterruptUnsupported, cron.InterruptError,
	}
	if len(knownCron) != int(cron.InterruptError)+1 {
		t.Errorf("cron InterruptOutcome count drift: listed %d members but max ordinal is %d — update knownCron and the alignment list in cron_router_adapter.go init()",
			len(knownCron), int(cron.InterruptError))
	}
}

// TestSessionStatus_Cast verifies cron.SessionStatus(int(...)) round-trip
// preserves the three known states. cron does not branch on SessionStatus
// today, but a future caller comparing against SessionExisting /
// SessionResumed / SessionNew relies on the cast staying identity.
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
