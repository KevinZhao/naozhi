package session

import (
	"testing"
	"time"
)

// TestCleanup_StuckThresholdFollowsTotalTimeout pins that Cleanup's
// stuck-running threshold is twice the configured total timeout, read from the
// spawn facet. 1 minute sits far below cli.DefaultTotalTimeout, so a Cleanup
// that ignored the configured value and fell back to the default would keep
// the silent session alive.
func TestCleanup_StuckThresholdFollowsTotalTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		silent time.Duration
		killed bool
	}{
		// 3m > 2×1m: stuck under the configured timeout, far inside twice the default.
		{"silent past twice the configured timeout", "key1", 3 * time.Minute, true},
		// 90s < 2×1m: a threshold below twice the timeout would kill it.
		{"silent within twice the configured timeout", "key1", 90 * time.Second, false},
		// A task: session's threshold is twice its 6h floor, not the 1m configured.
		{"task: session silent past twice the configured timeout", "task:abc", 3 * time.Minute, false},
		{"task: session silent past twice its own floor", "task:abc", 13 * time.Hour, true},
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
			s := injectSession(r, tc.key, proc)
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
