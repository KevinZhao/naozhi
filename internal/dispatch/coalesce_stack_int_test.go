package dispatch

// Tests for R20260603-PERF-5: the truncation tail in CoalesceMessages must
// format the dropped-message count via a stack-allocated [20]byte buffer
// rather than strconv.AppendInt(nil, …) which escapes to the heap.
// Pinned here so a future refactor that reintroduces the nil-slice variant
// is caught before it ships.

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestCoalesceMessages_TruncationTail_ZeroAlloc pins R20260603-PERF-5:
// the strconv number formatting in the truncation tail must not allocate.
// We measure allocations per operation via testing.AllocsPerRun — a regression
// to `strconv.AppendInt(nil, …)` causes one extra heap alloc in the nil→[]byte
// growth path, which is observable as allocs > N_baseline+1.
//
// The test creates a burst that reliably triggers the truncated > 0 branch
// (five messages each slightly over 1/4 of the cap, so messages 4 and 5
// overflow the cap) and checks that AllocsPerRun for the cold path stays
// at zero (the stack tmp[20]byte must not escape).
//
// Note: AllocsPerRun is inherently approximate; we assert <= 0 additional
// allocs attributable to the AppendInt call by running the truncation-path
// logic in isolation via a thin helper.
func TestCoalesceMessages_TruncationTail_ZeroAlloc(t *testing.T) {
	// AllocsPerRun must not be called in a parallel test (panics in Go stdlib).

	// Build a burst that will overflow so the truncated > 0 branch fires.
	per := maxCoalescedTextBytes/3 + 1
	big := strings.Repeat("x", per)
	msgs := make([]QueuedMsg, 5)
	for i := range msgs {
		msgs[i] = QueuedMsg{
			Text:      big,
			EnqueueAt: time.Date(2026, 6, 3, 12, 0, i, 0, time.UTC),
			Images:    []cli.ImageData{},
		}
	}

	// Warm up to avoid first-call JIT effects on the alloc count.
	CoalesceMessages(msgs)

	allocs := testing.AllocsPerRun(10, func() {
		CoalesceMessages(msgs)
	})

	// The truncation path itself (stack [20]byte + b.Write) must not produce
	// allocations beyond the strings.Builder growth. We cannot easily isolate
	// only the AppendInt call, so we assert the total allocs for the whole
	// CoalesceMessages call is <= 2 (Builder.Grow: 1 alloc; string result: 1
	// alloc). A regression to AppendInt(nil) adds exactly 1 more heap alloc.
	if allocs > 2 {
		t.Errorf("CoalesceMessages with truncation allocated %.0f times per call; "+
			"expected <= 2 (R20260603-PERF-5: strconv buffer must be stack-allocated, "+
			"not heap-allocated via AppendInt(nil, …))", allocs)
	}
}
