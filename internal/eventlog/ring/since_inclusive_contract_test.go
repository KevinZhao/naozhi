package ring

// The SinceInclusive↔EntriesSince contract lives here, not beside SinceInclusive
// in clievent: it needs the ring, and ring imports clievent, so asserting it from
// there would be an import cycle (#2649 G1-f). The pure-arithmetic half of the
// test stayed in clievent.

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestSinceInclusive_EntriesSinceReadmitsWatermark pins the contract the
// helper exists for: EntriesSince(clievent.SinceInclusive(T)) returns every entry AT T
// as well as newer ones, so a same-ms sibling is never dropped by a catch-up.
func TestSinceInclusive_EntriesSinceReadmitsWatermark(t *testing.T) {
	t.Parallel()
	log := NewEventLog(0)
	log.Append(clievent.EventEntry{Time: 1000, UUID: "old", Type: "user"})
	log.Append(clievent.EventEntry{Time: 2000, UUID: "a", Type: "thinking"})
	log.Append(clievent.EventEntry{Time: 2000, UUID: "b", Type: "text"})

	got := log.EntriesSince(clievent.SinceInclusive(2000))
	if len(got) != 2 || got[0].UUID != "a" || got[1].UUID != "b" {
		t.Fatalf("EntriesSince(clievent.SinceInclusive(2000)) = %+v, want [a b]", got)
	}
	if strict := log.EntriesSince(2000); len(strict) != 0 {
		t.Fatalf("sanity: strict EntriesSince(2000) = %+v, want empty", strict)
	}
}
