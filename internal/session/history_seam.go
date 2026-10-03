package session

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
)

// seamKey is an entry's identity at a tier seam. The persist bridge encodes
// the whole EventEntry, so memory and the naozhilog spool share UUIDs; a
// uuid-less entry is keyed by content, the same identity the dashboard's
// export pager uses.
func seamKey(e clievent.EventEntry) string {
	if e.UUID != "" {
		return e.UUID
	}
	return "\x00" + e.Type + "\x00" + e.Summary + "\x00" + e.Detail
}

// appendAtTime appends the entries of src whose Time is t to dst.
func appendAtTime(dst, src []clievent.EventEntry, t int64) []clievent.EventEntry {
	for _, e := range src {
		if e.Time == t {
			dst = append(dst, e)
		}
	}
	return dst
}

// dropHeld returns entries minus those at seamMS whose seamKey an entry of
// held at seamMS carries, as a fresh slice.
func dropHeld(entries []clievent.EventEntry, seamMS int64, held []clievent.EventEntry) []clievent.EventEntry {
	seen := make(map[string]struct{})
	for _, e := range held {
		if e.Time == seamMS {
			seen[seamKey(e)] = struct{}{}
		}
	}
	out := make([]clievent.EventEntry, 0, len(entries))
	for _, e := range entries {
		if e.Time == seamMS {
			if _, dup := seen[seamKey(e)]; dup {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// loadBeforeSeam reads the newest limit entries of src older than a seam the
// caller already holds part of: Time < seamMS, plus the entries at seamMS
// that held (any superset of the caller's seamMS entries) lacks. A strict
// LoadBefore(seamMS) would skip a same-millisecond sibling the caller does
// not have. The read asks for limit plus the held seamMS count and widens by
// the drop count while a full read still comes up short, so a short page
// means exhausted even when several disk entries share one held key.
func loadBeforeSeam(ctx context.Context, src history.Source, seamMS int64, held []clievent.EventEntry, limit int) ([]clievent.EventEntry, error) {
	n := limit
	for _, e := range held {
		if e.Time == seamMS {
			n++
		}
	}
	for {
		entries, err := src.LoadBefore(ctx, seamMS+1, n)
		if err != nil {
			return nil, err
		}
		sortEntriesByTimeStable(entries)
		fresh := dropHeld(entries, seamMS, held)
		if len(fresh) > limit {
			fresh = fresh[len(fresh)-limit:]
		}
		// limit+drops > n whenever fresh is short, so n strictly grows.
		if len(fresh) == limit || len(entries) < n {
			return fresh, nil
		}
		n = limit + len(entries) - len(fresh)
	}
}

// hasOlderThanSlice reports whether an entry the chronological slice lacks
// exists at or below its earliest millisecond (ring or disk): a probe that
// re-admits that millisecond and ignores the slice's own entries there,
// widened while a full read is all held keys (one key can match several
// stored entries). It fails OPEN when the probe degraded: a "load earlier"
// button on exhausted history is a benign no-op, a wrongly hidden one is
// unrecoverable.
func (s *ManagedSession) hasOlderThanSlice(ctx context.Context, entries []clievent.EventEntry) bool {
	oldest := entries[0].Time
	held := appendAtTime(nil, entries, oldest)
	n := len(held) + 1
	for {
		older, degraded := s.eventEntriesBeforeCtx(ctx, oldest+1, n)
		if degraded || len(dropHeld(older, oldest, held)) > 0 {
			return true
		}
		if len(older) < n {
			return false
		}
		n = len(older) + 1
	}
}
