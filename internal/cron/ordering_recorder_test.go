package cron

import (
	"sync"
	"testing"
)

// ordering_recorder_test.go — a shared, ordered event log so the fresh-context
// tests can assert WHEN something happened relative to something else. Epic I
// (#2547) of the arch review.
//
// The invariants these tests protect are all of the form "Reset must happen
// while the CAS gate is still held, i.e. BEFORE finishRun releases it". Before
// this recorder there was no way to observe that from behaviour: reapRouter
// appended Resets to its own slice and recordingBroadcaster appended run-ended
// events to another, and nothing related the two. So each invariant was pinned
// by a source-anchor test instead — a regexp over scheduler_run.go asserting
// that a `Reset(...)` match is followed by a `finishRun(finishArgs{` match
// before the next one.
//
// Those anchors fail on a rename, a reflow, or a helper extraction that changes
// nothing about the ordering, and they pass on a refactor that preserves the
// TEXT while breaking the ORDER — for instance moving the reap into a deferred
// closure, which still reads "Reset before finishRun" in source order and runs
// after it. One shared sequence makes the real thing observable.

// orderRecorder is an append-only sequence of named events shared by the test
// doubles in one scheduler run.
type orderRecorder struct {
	mu  sync.Mutex
	seq []string
}

func (o *orderRecorder) record(event string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.seq = append(o.seq, event)
	o.mu.Unlock()
}

func (o *orderRecorder) events() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.seq))
	copy(out, o.seq)
	return out
}

// firstIndex returns the position of the first event equal to name, or -1.
func (o *orderRecorder) firstIndex(name string) int {
	for i, e := range o.events() {
		if e == name {
			return i
		}
	}
	return -1
}

// assertCountBefore fails unless at least `want` occurrences of `before` precede
// the FIRST occurrence of `after`.
//
// Counting, not merely "one came first", is what makes this catch the real
// failure. The fresh-context invariant is that BOTH the preflight Reset and the
// error-path reap Reset land while the CAS gate is held, and the gate is released
// by the finishRun that emits run-ended. An assertion of the weaker form — "some
// reset precedes run-ended" — is satisfied by the preflight alone, so moving the
// reap into a deferred closure (which runs at function exit, i.e. AFTER
// finishRun) passes it. Verified: that mutation slips through the weak form and
// is caught by this one.
func (o *orderRecorder) assertCountBefore(t *testing.T, before string, want int, after, what string) {
	t.Helper()
	seq := o.events()
	got := 0
	for _, e := range seq {
		if e == after {
			if got < want {
				t.Errorf("%s: only %d %q before the first %q, want >=%d\n  sequence: %v",
					what, got, before, after, want, seq)
			}
			return
		}
		if e == before {
			got++
		}
	}
	t.Errorf("%s: %q never happened\n  sequence: %v", what, after, seq)
}
