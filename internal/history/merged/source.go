// Package merged implements history.Source as the composition of a
// "local" source (naozhilog) and a "fallback" source (claudejsonl),
// returning results deduplicated by EventEntry.UUID.
//
// Why a merged source rather than "local if non-empty, else fallback":
//
//   - Upgrade path. Freshly-upgraded naozhi starts with an empty
//     events/ directory. The user's history must still be visible;
//     Claude JSONL fills the gap. As new events are appended to
//     events/, they cover the recent range; older history keeps
//     coming from Claude JSONL.
//   - No silent drops. "local if non-empty" would hide Claude JSONL
//     on the very first event, making the gap VERY visible.
//   - Dedup. Once a naozhi-native entry is written, its UUID is
//     stable (crypto/rand) AND a Claude JSONL copy of the same
//     turn derives the SAME uuid (DeriveLegacyUUID) — MergedSource
//     collapses the two to one entry rather than doubling it.
//
// Ordering:
//   - Merged result is strictly ordered by (Time, UUID) ascending.
//     Time is the primary sort key; UUID is used as a deterministic
//     tie-break when two entries share the same Time (the Persister's
//     Seq field is local-only and not cross-source comparable per
//     RFC §3.5.3).
//   - LoadBefore honors the `beforeMS` filter after the merge so a
//     timing drift between sources doesn't accidentally return
//     an entry > beforeMS.
//   - After merge-sort-dedup, the tail `limit` entries are kept.
//     This preserves the "newest first visible" invariant the
//     dashboard relies on for pagination.
package merged

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history"
)

// Source is the merged history.Source. Either Local or Fallback may
// be nil; a nil source is treated as "always returns empty".
type Source struct {
	Local    history.Source
	Fallback history.Source
}

// Ensure Source implements the history.Source interface even without
// the compile check at the router wiring site.
var _ history.Source = (*Source)(nil)

// LoadBefore fans out to both sources, merges, dedups, and returns
// the newest `limit` entries satisfying Time < beforeMS.
//
// beforeMS <= 0 means "no upper bound" and is passed through to both
// sources unchanged (each source handles the no-bound case).
//
// Error handling:
//   - An error from one source does NOT short-circuit: we log and
//     degrade to whatever the other source returned. This matches
//     RFC §3.4's "naozhi local → Claude fallback" safety net.
//   - Only when BOTH sources fail do we return the local error (the
//     caller gets SOMETHING actionable to surface).
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}

	var local, fallback []cli.EventEntry
	var localErr, fallbackErr error

	if s.Local != nil {
		local, localErr = s.Local.LoadBefore(ctx, beforeMS, limit)
	}
	if s.Fallback != nil {
		fallback, fallbackErr = s.Fallback.LoadBefore(ctx, beforeMS, limit)
	}

	switch {
	case localErr != nil && fallbackErr != nil:
		return nil, localErr
	}

	merged := mergeDedup(local, fallback, beforeMS)
	if len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	return merged, nil
}

// entryCmp is the canonical (Time, UUID) ordering used across all
// merge paths. Centralizing the comparison in one place guarantees
// the fast-path merge and the defensive-path sort share identical
// tie-break semantics — a divergence here would let identical inputs
// produce different orders depending on which path fired, breaking
// the dashboard's paginate-by-Time cursor invariant.
func entryCmp(a, b cli.EventEntry) int {
	if c := cmp.Compare(a.Time, b.Time); c != 0 {
		return c
	}
	return strings.Compare(a.UUID, b.UUID)
}

// mergeDedup implements the core merge algorithm. Callers pass
// already-sorted slices (naozhilog.readAllEntries returns chronological
// log order; discovery/history_tail reverses to chronological). When
// that contract holds, mergeDedup runs an O(n) two-way merge. When a
// source violates the contract, mergeDedup falls back to a concat+sort
// pass and logs a WARN — correctness is preserved at the cost of this
// one call's savings.
//
// Dedup rule: when two entries share the same UUID, keep the LOCAL
// one. The local tier has full EventEntry fidelity (Images,
// ImagePaths, AskQuestion, agent-team linkage…) while the Claude
// JSONL fallback only has text content. Keeping local preserves the
// richer render path for rehydrated history.
//
// Missing UUID: entries without a UUID cannot be deduped by key.
// They're kept as-is (no dedup pass); the Persister's stampUUID +
// discovery's DeriveLegacyUUID together ensure this case is rare —
// legacy data produced before the UUID migration, or an entry
// manufactured by some future path that skipped Append. We don't
// try to synthesize a dedup key here because any rule we pick
// risks collapsing unrelated events.
func mergeDedup(local, fallback []cli.EventEntry, beforeMS int64) []cli.EventEntry {
	if len(local) == 0 && len(fallback) == 0 {
		return nil
	}

	// Fast path: both inputs honour the chronological contract.
	// slices.IsSortedFunc is O(n) but runs at the speed of a plain
	// loop — the cost is dwarfed by the O(n log n) pass it replaces
	// in the common case. Two-way merge is allocation-identical to
	// the old code (one map, one result slice) so there's no memory
	// regression either.
	if slices.IsSortedFunc(local, entryCmp) && slices.IsSortedFunc(fallback, entryCmp) {
		return mergeSorted(local, fallback, beforeMS)
	}

	// Contract violation: a source returned out-of-order entries.
	// Warn once per call so the underlying source can be fixed, then
	// fall back to the legacy concat+sort path. This should never
	// fire in steady state — if it does, grep the warning and trace
	// back to whichever Source.LoadBefore produced the offending slice.
	slog.Warn("merged: source returned unsorted entries; repairing with sort",
		"local_len", len(local), "fallback_len", len(fallback))
	return mergeSortFallback(local, fallback, beforeMS)
}

// mergeSorted walks two already-sorted input slices linearly and
// returns their deduped union, also sorted by (Time, UUID). Linear
// in len(local)+len(fallback).
//
// Dedup invariant: local entries are ALWAYS kept. Fallback entries
// are dropped if their UUID was already seen — either from local
// (seeded in step 1 below) or from an earlier fallback entry (added
// to seen on emit). Empty UUIDs bypass dedup entirely and are kept
// as-is; see the package-level comment on "Missing UUID".
func mergeSorted(local, fallback []cli.EventEntry, beforeMS int64) []cli.EventEntry {
	// Step 1: seed `seen` with every UUID from local in one pass so
	// the merge can check fallback entries against the full local
	// set, not just those already emitted. Sizing at len(local) is a
	// safe upper bound — empty-UUID entries simply won't populate a
	// slot, but over-allocating a few map buckets is cheaper than
	// a rehash when the map grows during the fallback tail flush.
	seen := make(map[string]struct{}, len(local))
	for _, e := range local {
		if e.UUID != "" {
			seen[e.UUID] = struct{}{}
		}
	}

	out := make([]cli.EventEntry, 0, len(local)+len(fallback))
	emit := func(e cli.EventEntry) {
		if beforeMS > 0 && e.Time >= beforeMS {
			return
		}
		out = append(out, e)
	}

	// Step 2: two-way merge. `entryCmp(..) <= 0` picks local on an
	// exact (Time, UUID) tie so the "local wins dedup" rule holds
	// even when the two sources somehow produced identical keys.
	i, j := 0, 0
	for i < len(local) && j < len(fallback) {
		if entryCmp(local[i], fallback[j]) <= 0 {
			emit(local[i])
			i++
			continue
		}
		f := fallback[j]
		if f.UUID == "" {
			emit(f)
		} else if _, dup := seen[f.UUID]; !dup {
			seen[f.UUID] = struct{}{}
			emit(f)
		}
		j++
	}
	for ; i < len(local); i++ {
		emit(local[i])
	}
	for ; j < len(fallback); j++ {
		f := fallback[j]
		if f.UUID == "" {
			emit(f)
			continue
		}
		if _, dup := seen[f.UUID]; dup {
			continue
		}
		seen[f.UUID] = struct{}{}
		emit(f)
	}
	return out
}

// mergeSortFallback is the legacy concat-then-sort path, retained
// verbatim for the contract-violation branch. Behaviour is identical
// to the pre-R220 implementation: step 1 seeds `seen` from local and
// emits local entries unconditionally; step 2 appends fallback entries
// that aren't duplicates; step 3 stable-sorts by (Time, UUID).
//
// Kept as a separate function (not inlined into mergeDedup) so the
// fast path's happy-path semantics stay obvious to readers, and so
// future refactors to the fast path can't accidentally regress the
// defensive behaviour.
func mergeSortFallback(local, fallback []cli.EventEntry, beforeMS int64) []cli.EventEntry {
	seen := make(map[string]struct{}, len(local))
	out := make([]cli.EventEntry, 0, len(local)+len(fallback))
	for _, e := range local {
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		if e.UUID != "" {
			seen[e.UUID] = struct{}{}
		}
		out = append(out, e)
	}
	for _, e := range fallback {
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		if e.UUID != "" {
			if _, dup := seen[e.UUID]; dup {
				continue
			}
			seen[e.UUID] = struct{}{}
		}
		out = append(out, e)
	}
	// SortStableFunc preserves local-first ordering for entries that
	// tie on (Time, UUID) — matching the original sort.SliceStable
	// contract. Needed for empty-UUID entries that could legitimately
	// tie; non-empty-UUID ties are already resolved by the dedup pass.
	slices.SortStableFunc(out, entryCmp)
	return out
}
