package session

import (
	"testing"
	"time"
)

// TestCleanup_StuckThresholdFollowsTotalTimeout pins that Cleanup's
// stuck-running threshold is twice the configured total timeout, read from the
// spawn facet. Every other Cleanup test configures 5 minutes, which is also
// cli.DefaultTotalTimeout, so a Cleanup that ignored the configured value and
// fell back to the default would pass them all; 1 minute tells the two apart.
func TestCleanup_StuckThresholdFollowsTotalTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		silent time.Duration
		killed bool
	}{
		// 3m > 2×1m: stuck under the configured timeout, not under the 10m default.
		{"silent past twice the configured timeout", 3 * time.Minute, true},
		// 90s < 2×1m: a threshold below twice the timeout would kill it.
		{"silent within twice the configured timeout", 90 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Router{
				ss:       newSessionTable(),
				maxProcs: 3,
				ttl:      time.Hour,
				pruneTTL: 72 * time.Hour,
				spawn:    spawnConfig{totalTimeout: time.Minute},
			}
			proc := newRunningProc()
			s := injectSession(r, "key1", proc)
			s.lastActive.Store(time.Now().Add(-tc.silent).UnixNano())

			r.Cleanup()

			if got := !proc.Alive(); got != tc.killed {
				t.Fatalf("running session silent for %v with totalTimeout 1m: killed = %v, want %v", tc.silent, got, tc.killed)
			}
			want := ""
			if tc.killed {
				want = "stuck_running"
			}
			if got := loadAtomicString(&s.deathReason); got != want {
				t.Errorf("deathReason = %q, want %q", got, want)
			}
		})
	}
}
