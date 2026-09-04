package session

import "github.com/naozhi/naozhi/internal/cli"

// meteringCache is the last MeteringUsage view Snapshot built for a live
// process, keyed by (process identity, MeteringGen): the dashboard polls
// Snapshot at 1 Hz × N tabs × M sessions while metering rows change at most
// once per turn (#2345). The cached slice is shared READ-ONLY across snapshots
// — nothing may mutate SessionSnapshot.MeteringUsage.
type meteringCache struct {
	proc    processIface
	gen     uint64
	usage   []cli.MeteringEntry
	credits float64
}

// meteringView returns proc's metering rows plus their credit-unit sum,
// reusing the previous result while proc.MeteringGen() is unchanged.
//
// Ordering: the gen is sampled BEFORE MeteringUsage copies the rows, so a
// concurrent write can only tag the cache with an older gen (one extra rebuild)
// — never a fresh gen on stale rows. Keyed by process identity too: a respawned
// process restarts its counter, so an equal gen on a different process must
// miss. gen == 0 bypasses the cache so callers always see live data.
func (s *ManagedSession) meteringView(proc processIface) ([]cli.MeteringEntry, float64) {
	gen := proc.MeteringGen()
	if gen == 0 {
		usage := proc.MeteringUsage()
		return usage, sumMeteringCredits(usage)
	}
	if c := s.meteringCache.Load(); c != nil && c.gen == gen && c.proc == proc {
		return c.usage, c.credits
	}
	usage := proc.MeteringUsage()
	c := &meteringCache{proc: proc, gen: gen, usage: usage, credits: sumMeteringCredits(usage)}
	s.meteringCache.Store(c)
	return c.usage, c.credits
}

// sumMeteringCredits totals the credit-typed rows. Zero when no row carries a
// credit unit, so callers can gate the TotalCost override on > 0.
func sumMeteringCredits(usage []cli.MeteringEntry) float64 {
	var credits float64
	for _, m := range usage {
		if m.Unit == "credit" || m.Unit == "credits" {
			credits += m.Value
		}
	}
	return credits
}
