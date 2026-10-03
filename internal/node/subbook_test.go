package node

import (
	"slices"
	"testing"
)

func TestSubBook_AddSeedsWatermarkOnlyForFirstSink(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	s1, s2 := &mockSink{id: 1}, &mockSink{id: 2}

	if !b.add(s1, "k", 500) {
		t.Fatal("first add on an empty key must report first=true")
	}
	if b.add(s1, "k", 900) {
		t.Fatal("re-adding the same sink must report first=false")
	}
	if n := len(b.subs["k"]); n != 1 {
		t.Fatalf("same-sink re-add left %d entries, want 1", n)
	}
	if b.add(s2, "k", 100) {
		t.Fatal("second sink must report first=false")
	}
	if got := b.lastEvent["k"]; got != 500 {
		t.Fatalf("lastEvent = %d, want the first sink's seed 500 (later adds must not move it)", got)
	}
}

func TestSubBook_ObserveIsMonotonicAndIgnoresUnheldKeys(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	b.add(&mockSink{id: 1}, "k", 500)

	for _, step := range []struct {
		t    int64
		want int64
	}{{1200, 1200}, {800, 1200}, {0, 1200}, {1500, 1500}} {
		b.observe("k", step.t)
		if got := b.lastEvent["k"]; got != step.want {
			t.Fatalf("after observe(%d): lastEvent = %d, want %d", step.t, got, step.want)
		}
	}

	b.observe("nobody", 42)
	if _, ok := b.lastEvent["nobody"]; ok {
		t.Fatal("observe on a key without sinks grew lastEvent")
	}
}

func TestSubBook_RemoveForgetsWatermarkWithLastSink(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	s1, s2 := &mockSink{id: 1}, &mockSink{id: 2}
	b.add(s1, "k", 500)
	b.add(s2, "k", 0)

	if b.remove(s1, "k") {
		t.Fatal("remove reported empty while s2 still holds the key")
	}
	if _, ok := b.lastEvent["k"]; !ok {
		t.Fatal("watermark dropped while a sink still holds the key")
	}
	if !b.remove(s2, "k") {
		t.Fatal("removing the last sink must report empty")
	}
	if _, ok := b.lastEvent["k"]; ok {
		t.Fatal("watermark survived its last sink")
	}
}

func TestSubBook_RemoveAllReturnsEmptiedKeys(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	s1, s2 := &mockSink{id: 1}, &mockSink{id: 2}
	b.add(s1, "solo", 10)
	b.add(s1, "shared", 20)
	b.add(s2, "shared", 0)

	emptied := b.removeAll(s1)
	if !slices.Equal(emptied, []string{"solo"}) {
		t.Fatalf("removeAll emptied %v, want [solo]", emptied)
	}
	if _, ok := b.lastEvent["solo"]; ok {
		t.Fatal("emptied key kept its watermark")
	}
	if got := b.lastEvent["shared"]; got != 20 {
		t.Fatalf("shared watermark = %d, want 20", got)
	}
}

func TestSubBook_DropAndReset(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	b.add(&mockSink{id: 1}, "a", 1)
	b.add(&mockSink{id: 1}, "b", 2)

	b.drop("a")
	if b.has("a") || b.lastEvent["a"] != 0 {
		t.Fatal("drop left key a behind")
	}
	if !b.has("b") {
		t.Fatal("drop removed an unrelated key")
	}
	b.reset()
	if len(b.subs) != 0 || len(b.lastEvent) != 0 {
		t.Fatalf("reset left subs=%d lastEvent=%d", len(b.subs), len(b.lastEvent))
	}
}

func TestSubBook_ResubscribeListCarriesWatermarks(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	b.add(&mockSink{id: 1}, "a", 100)
	b.add(&mockSink{id: 2}, "b", 0)
	b.observe("b", 700)

	got := b.resubscribeList()
	slices.SortFunc(got, func(x, y resubscription) int {
		if x.key < y.key {
			return -1
		}
		return 1
	})
	want := []resubscription{{key: "a", after: 100}, {key: "b", after: 700}}
	if !slices.Equal(got, want) {
		t.Fatalf("resubscribeList = %+v, want %+v", got, want)
	}
}

func TestSubBook_SnapshotIsDetachedAndReleaseClearsPointers(t *testing.T) {
	t.Parallel()
	b := newSubBook()
	s1, s2 := &mockSink{id: 1}, &mockSink{id: 2}
	b.add(s1, "k", 0)
	b.add(s2, "k", 0)

	snap := b.snapshot("k")
	b.remove(s1, "k")
	if got := *snap; len(got) != 2 || got[0] != s1 || got[1] != s2 {
		t.Fatalf("snapshot changed with the book: %v", got)
	}
	backing := (*snap)[:2]
	releaseSnapshot(snap)
	if backing[0] != nil || backing[1] != nil {
		t.Fatal("releaseSnapshot left sink pointers in the pooled array")
	}
}

func TestWSRelay_ForwardEventAdvancesOnlyHeldKeys(t *testing.T) {
	t.Parallel()
	r := newWSRelay(&HTTPClient{ID: "n1"})
	r.mu.Lock()
	r.book.add(&mockSink{id: 1}, "held", 500)
	r.mu.Unlock()

	r.forwardEvent([]byte(`{"type":"event","key":"held","event":{"time":900}}`))
	r.forwardEvent([]byte(`{"type":"event","key":"held","event":{"time":600}}`))
	r.forwardEvent([]byte(`{"type":"event","key":"gone","event":{"time":42}}`))

	r.mu.Lock()
	defer r.mu.Unlock()
	if got := r.book.lastEvent["held"]; got != 900 {
		t.Fatalf("held watermark = %d, want 900", got)
	}
	if _, ok := r.book.lastEvent["gone"]; ok {
		t.Fatal("an event for a key nobody holds created a watermark")
	}
}
