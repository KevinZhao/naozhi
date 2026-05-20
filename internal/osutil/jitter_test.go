package osutil

import (
	"testing"
	"time"
)

// TestJitterBackoff replaces three byte-for-byte identical TestJitterBackoff
// copies that previously lived in internal/node, internal/platform, and
// internal/upstream. Those wrappers were thin pass-throughs to this single
// implementation; consolidating their tests here keeps the contract — d <= 0
// passes through, positive d scales by [0.75, 1.25) — pinned without three
// duplicate suites drifting. (R228-ARCH-6)
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
			// d <= 0 must passthrough verbatim so callers using a
			// zero-duration "no backoff" sentinel don't pick up a
			// non-zero jittered value and stall.
			if tc.in <= 0 {
				if got := JitterBackoff(tc.in); got != tc.in {
					t.Fatalf("JitterBackoff(%v) = %v, want passthrough", tc.in, got)
				}
				return
			}
			// Factor range is [0.75, 1.25). 200 samples is enough to hit
			// both ends of the range in practice without flaking.
			lo := time.Duration(float64(tc.in) * 0.75)
			hi := time.Duration(float64(tc.in) * 1.25)
			for i := 0; i < 200; i++ {
				got := JitterBackoff(tc.in)
				if got < lo || got >= hi {
					t.Fatalf("sample %d: JitterBackoff(%v) = %v, want in [%v,%v)", i, tc.in, got, lo, hi)
				}
			}
		})
	}
}
