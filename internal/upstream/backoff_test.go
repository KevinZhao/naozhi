package upstream

import (
	"testing"
	"time"
)

// TestJitterBackoff mirrors internal/platform/backoff_test.go and
// internal/node/backoff_test.go. The three packages carry byte-for-byte
// identical jitterBackoff implementations (Round 156 audit: 3 copies,
// shared consolidation deferred to an osutil extraction). Until that
// refactor lands, each copy has its own unit test so drift in any one
// copy (e.g. someone accidentally changes the [0.75, 1.25) range here
// only) fails a local test rather than hiding behind the other packages.
func TestJitterBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
		{"small", 10 * time.Millisecond},
		{"medium", 500 * time.Millisecond},
		{"large", 30 * time.Second},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// d <= 0 arm: must passthrough verbatim so caller-level
			// retry loops that use a zero-duration "no backoff" sentinel
			// don't pick up a non-zero jittered value and stall.
			if tc.in <= 0 {
				if got := jitterBackoff(tc.in); got != tc.in {
					t.Fatalf("jitterBackoff(%v) = %v, want passthrough", tc.in, got)
				}
				return
			}
			// Factor range is [0.75, 1.25) — the upper bound is open
			// because rand.Float64() is [0, 1) so factor=0.75+1*0.5=1.25
			// is unreachable. 200 samples is enough to hit both ends of
			// the range in practice without flaking.
			lo := time.Duration(float64(tc.in) * 0.75)
			hi := time.Duration(float64(tc.in) * 1.25)
			for i := 0; i < 200; i++ {
				got := jitterBackoff(tc.in)
				if got < lo || got >= hi {
					t.Fatalf("sample %d: jitterBackoff(%v) = %v, want in [%v,%v)", i, tc.in, got, lo, hi)
				}
			}
		})
	}
}
