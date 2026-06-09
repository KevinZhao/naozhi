package session

import "testing"

// TestInterruptOutcomeOrdinals pins the *integer* values of every
// InterruptOutcome constant.
//
// R20260527122801-CR-3 (#1312): InterruptOutcome is the source-of-truth
// enum for an ordinal that crosses three layers. cmd/naozhi's
// cron_router_adapter.go casts session.InterruptOutcome → cron.InterruptOutcome
// with a bare numeric conversion (cron.InterruptOutcome(int(o))), guarded only
// by an init()-time panic that fires when the binary boots. That guard:
//
//   - depends on the binary actually being run (or cmd/naozhi being built),
//     so a session-side reorder can slip through `go test ./internal/session/`;
//   - the existing String() test pins the *names* but not the *values*, so a
//     reorder that keeps each (value→name) switch arm consistent passes it
//     while silently shifting the wire ordinal the cron cast depends on.
//
// This test runs in CI on every session package change and fails the instant
// any constant's integer value moves, catching the drift at the layer that
// owns the enum rather than only at boot. If you intentionally renumber these,
// update internal/wireup/cron_router_adapter.go's init() pin AND cron's mirror enum
// (internal/cron/agent_opts.go) in the same change.
func TestInterruptOutcomeOrdinals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  InterruptOutcome
		want int
	}{
		{"InterruptSent", InterruptSent, 0},
		{"InterruptNoSession", InterruptNoSession, 1},
		{"InterruptNoTurn", InterruptNoTurn, 2},
		{"InterruptUnsupported", InterruptUnsupported, 3},
		{"InterruptError", InterruptError, 4},
	}
	for _, tc := range cases {
		if int(tc.got) != tc.want {
			t.Errorf("%s ordinal = %d, want %d — cron numeric cast in "+
				"internal/wireup/cron_router_adapter.go relies on this value",
				tc.name, int(tc.got), tc.want)
		}
	}
}
