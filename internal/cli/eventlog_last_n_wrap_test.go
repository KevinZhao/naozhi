package cli

// R093146-CLI-3: LastNAppend branch-on-wrap optimisation pin tests.
// Cover three ring topologies:
//   1. No wrap: start+count <= maxSize — single copy path.
//   2. Wrap: head sits in the middle of the ring so the window straddles
//      the array boundary — two-copy path.
//   3. count == maxSize: full ring read, head at mid-ring (always wraps).
//
// Each case compares the optimised output against the reference loop to
// prove semantic equivalence.

import (
	"testing"
)

// referenceLastN is the original modulo-per-step loop used before the
// branch-on-wrap optimisation. It exists solely to cross-check the fast
// path; it is NOT part of production code.
func referenceLastN(l *EventLog, n int) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := l.count
	if n > 0 && n < count {
		count = n
	}
	dst := make([]EventEntry, count)
	start := (l.head - count + l.maxSize) % l.maxSize
	for i := 0; i < count; i++ {
		dst[i] = l.entries[(start+i)%l.maxSize]
	}
	return dst
}

func TestLastNAppend_BranchOnWrap(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		maxSize int
		// times to append in order
		times []int64
		// n passed to LastN (0 = all)
		n int
	}

	cases := []tc{
		{
			name:    "no_wrap_partial",
			maxSize: 8,
			// append 4, head ends at 4; start = 4-3 = 1; start+count=4 <= 8
			times: []int64{10, 20, 30, 40},
			n:     3,
		},
		{
			name:    "no_wrap_all",
			maxSize: 8,
			// 5 entries, head=5, start=0; 0+5=5 <= 8
			times: []int64{10, 20, 30, 40, 50},
			n:     0,
		},
		{
			name:    "wrap_head_mid",
			maxSize: 4,
			// Fill ring: 4 entries, then add 2 more so head=2, ring is [50,60,30,40]
			// count=4, start=(2-4+4)%4=2; start+count=6 > 4 => wraps
			times: []int64{10, 20, 30, 40, 50, 60},
			n:     0,
		},
		{
			name:    "wrap_partial_n",
			maxSize: 4,
			// Same ring state as above; ask for last 3
			times: []int64{10, 20, 30, 40, 50, 60},
			n:     3,
		},
		{
			name:    "full_ring_count_equals_maxSize",
			maxSize: 5,
			// Append exactly 5 entries, no overwrite yet. head=5%5=0, count=5.
			// start=(0-5+5)%5=0; 0+5=5 <= 5 — no wrap.
			times: []int64{1, 2, 3, 4, 5},
			n:     0,
		},
		{
			name:    "full_ring_with_overwrite",
			maxSize: 5,
			// Append 7: head=7%5=2, count=5.
			// start=(2-5+5)%5=2; 2+5=7 > 5 => wraps.
			times: []int64{1, 2, 3, 4, 5, 6, 7},
			n:     0,
		},
		{
			name:    "count_one",
			maxSize: 4,
			times:   []int64{42},
			n:       0,
		},
		{
			name:    "empty_log",
			maxSize: 4,
			times:   nil,
			n:       0,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l := NewEventLog(c.maxSize)
			for _, ts := range c.times {
				l.Append(EventEntry{Time: ts, Type: "text"})
			}

			want := referenceLastN(l, c.n)
			got := l.LastN(c.n)

			if len(want) != len(got) {
				t.Fatalf("len mismatch: want %d got %d", len(want), len(got))
			}
			for i := range want {
				if want[i].Time != got[i].Time {
					t.Errorf("entry[%d] Time: want %d got %d", i, want[i].Time, got[i].Time)
				}
			}
		})
	}
}
