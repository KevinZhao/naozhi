package costledger

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// tally counts deliveries per RunID; the subscriber runs on the writer.
type tally struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *tally) add(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[e.RunID]++
}

func (c *tally) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

func durable(s *Store) int {
	n := 0
	_ = s.Scan(Query{From: t0.Add(-72 * time.Hour), To: t0.Add(time.Hour)}, func(Entry) bool { n++; return true })
	return n
}

// waitDurable polls until n entries are on disk; false on timeout.
func waitDurable(s *Store, n int, timeout time.Duration) bool {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(timeout)
	for durable(s) < n {
		select {
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
	return true
}

func tagged(ts time.Time, run string) Entry {
	e := mk(ts, SourceSession, UnitUSD, 1)
	e.RunID = run
	return e
}

// A batch the writer lands while Subscribe is between its replay and its
// registration must still reach the subscriber, once: the replay holds the
// writer, so the batch is delivered live instead of falling in the gap.
func TestSubscribe_ReplayThenLiveDeliversEachEntryOnce(t *testing.T) {
	s, _ := newTestStore(t, t0)
	from := t0.Add(-time.Hour)
	for _, e := range []Entry{tagged(t0.Add(-25*time.Hour), "yesterday"), tagged(from.Add(-time.Nanosecond), "before-from"), tagged(from, "at-from"), tagged(t0, "on-disk")} {
		s.Append(e)
	}
	if !waitDurable(s, 4, 5*time.Second) {
		t.Fatal("seed entries never reached disk")
	}
	s.replayedHook = func() {
		for i := range batchMax {
			s.Append(tagged(t0, fmt.Sprintf("gap-%d", i)))
		}
		waitDurable(s, 4+batchMax, 300*time.Millisecond)
	}
	got := &tally{n: map[string]int{}}
	s.Subscribe(from, got.add)
	s.Append(tagged(t0, "after"))
	s.Close()

	want := map[string]int{"at-from": 1, "on-disk": 1, "after": 1}
	for i := range batchMax {
		want[fmt.Sprintf("gap-%d", i)] = 1
	}
	have := got.snapshot()
	for run, n := range want {
		if have[run] != n {
			t.Errorf("%s delivered %d times, want %d", run, have[run], n)
		}
	}
	for run, n := range have {
		if _, ok := want[run]; !ok {
			t.Errorf("%s delivered %d times; it is before from and must not be", run, n)
		}
	}
}

// A read-only (or closed) store replays what is on disk and registers
// nothing; a disabled or nil store never calls fn.
func TestSubscribe_ReadOnlyReplaysDisabledNever(t *testing.T) {
	s, dir := newTestStore(t, t0)
	s.Append(tagged(t0, "a"))
	s.Close()

	ro := OpenReadOnly(dir, Options{Now: func() time.Time { return t0 }})
	got := &tally{n: map[string]int{}}
	ro.Subscribe(t0.Add(-time.Hour), got.add)
	if have := got.snapshot(); len(have) != 1 || have["a"] != 1 {
		t.Errorf("read-only replay = %v, want a once", have)
	}
	if len(ro.subs) != 0 {
		t.Errorf("read-only store registered %d subscribers; nothing will ever feed them", len(ro.subs))
	}

	called := false
	NewStore("", Options{}).Subscribe(t0, func(Entry) { called = true })
	var nilStore *Store
	nilStore.Subscribe(t0, func(Entry) { called = true })
	if called {
		t.Error("a disabled store called its subscriber")
	}
}
