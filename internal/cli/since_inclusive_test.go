package cli

import "testing"

func TestSinceInclusive(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want int64 }{
		{0, 0},
		{-5, -5},
		{1, 0},
		{1700000000000, 1699999999999},
	}
	for _, c := range cases {
		if got := SinceInclusive(c.in); got != c.want {
			t.Errorf("SinceInclusive(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSinceInclusive_EntriesSinceReadmitsWatermark pins the contract the
// helper exists for: EntriesSince(SinceInclusive(T)) returns every entry AT T
// as well as newer ones, so a same-ms sibling is never dropped by a catch-up.
func TestSinceInclusive_EntriesSinceReadmitsWatermark(t *testing.T) {
	t.Parallel()
	log := NewEventLog(0)
	log.Append(EventEntry{Time: 1000, UUID: "old", Type: "user"})
	log.Append(EventEntry{Time: 2000, UUID: "a", Type: "thinking"})
	log.Append(EventEntry{Time: 2000, UUID: "b", Type: "text"})

	got := log.EntriesSince(SinceInclusive(2000))
	if len(got) != 2 || got[0].UUID != "a" || got[1].UUID != "b" {
		t.Fatalf("EntriesSince(SinceInclusive(2000)) = %+v, want [a b]", got)
	}
	if strict := log.EntriesSince(2000); len(strict) != 0 {
		t.Fatalf("sanity: strict EntriesSince(2000) = %+v, want empty", strict)
	}
}
