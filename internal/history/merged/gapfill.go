package merged

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// GapFill returns the fallback turns hidden behind local's persist_gap
// records, oldest first. persist writes a gap record (no UUID, Time of the
// next stored batch) when it dropped batches, so the lost turns sit between
// the previous local entry and the record. A fallback row is returned when its
// Time lies in such a window widened by the content skew bounds, it has no
// UUID or content twin in local, and Time >= floor. local is in file order and
// may reach below floor so a record just under the caller's cut still counts.
// Without a gap record the fallback is not read; a failed read returns nil.
func (s *Source) GapFill(ctx context.Context, local []clievent.EventEntry, floor int64, limit int) []clievent.EventEntry {
	if s == nil || s.Fallback == nil || limit <= 0 {
		return nil
	}
	wins := gapWindows(local)
	if len(wins) == 0 {
		return nil
	}
	fb, err := s.Fallback.LoadBefore(ctx, 0, limit)
	if err != nil {
		slog.Warn("merged: gap fill fallback read failed", "err", err)
		return nil
	}
	fb = slices.Clone(fb)
	slices.SortStableFunc(fb, func(a, b clievent.EventEntry) int { return cmp.Compare(a.Time, b.Time) })
	seen := make(map[string]struct{}, len(local))
	for _, e := range local {
		if e.UUID != "" {
			seen[e.UUID] = struct{}{}
		}
	}
	// pairContent must see exactly the local UUID set, so it runs before the
	// loop below records fallback UUIDs.
	twins := pairContent(local, fb, 0, seen)
	var out []clievent.EventEntry
	for i, f := range fb {
		if f.UUID != "" {
			if _, dup := seen[f.UUID]; dup {
				continue
			}
			seen[f.UUID] = struct{}{}
		}
		if _, twin := twins[i]; twin {
			continue
		}
		if f.Time >= floor && inGapWindow(wins, f.Time) {
			out = append(out, f)
		}
	}
	return out
}

// gapWindow is an inclusive Time range a dropped turn's fallback twin may hold.
type gapWindow struct{ lo, hi int64 }

// gapWindows derives one window per gap record: from the previous non-gap
// entry minus the lag bound (an assistant twin is stamped earlier than its
// local copy) to the record plus the lead bound (a user twin is stamped
// later). A record with nothing before it has no lower bound.
func gapWindows(local []clievent.EventEntry) []gapWindow {
	var out []gapWindow
	lo := int64(minTime)
	for _, e := range local {
		if e.Type != clievent.KindPersistGap {
			lo = e.Time - contentSkewLagMS
			continue
		}
		out = append(out, gapWindow{lo: lo, hi: e.Time + contentSkewLeadMS})
	}
	return out
}

// minTime is the open lower bound of a window with no entry before it.
const minTime = -1 << 62

func inGapWindow(wins []gapWindow, t int64) bool {
	for _, w := range wins {
		if t >= w.lo && t <= w.hi {
			return true
		}
	}
	return false
}
