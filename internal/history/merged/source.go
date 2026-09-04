// Package merged implements history.Source as the composition of a "local"
// source (naozhilog) and a "fallback" source (claudejsonl), deduplicated.
//
// Both tiers are always consulted: after an upgrade events/ starts empty and
// Claude JSONL must fill the gap, while new events cover the recent range.
// The two tiers can describe the SAME turn with DIFFERENT UUIDs (naozhi stamps
// crypto/rand UUIDs; Claude JSONL carries Claude's message uuid), so a
// fallback entry is dropped on EITHER an exact UUID match OR a matching
// content key (Type, Detail) whose timestamps sit inside a directional skew
// window — see contentKey, pairContent and skewWindowFor. The content key
// must NOT include Time: the tiers stamp the same turn at different pipeline
// points.
//
// The merged result is ordered by (Time, UUID) ascending, filtered to
// Time < beforeMS after the merge, and trimmed to the newest `limit`.
package merged

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Source is the merged history.Source. Either Local or Fallback may
// be nil; a nil source is treated as "always returns empty".
type Source struct {
	Local    history.Source
	Fallback history.Source
}

var _ history.Source = (*Source)(nil)

// LoadBefore fans out to both sources, merges, dedups, and returns the newest
// `limit` entries with Time < beforeMS (beforeMS <= 0: no upper bound). An
// error from one source does not short-circuit: it is logged and the other
// tier's result is used. Only when both fail is the local error returned.
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}

	var local, fallback []clievent.EventEntry
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
	case localErr != nil:
		slog.Warn("merged: local source failed; degrading to fallback", "err", localErr)
	case fallbackErr != nil:
		slog.Warn("merged: fallback source failed; degrading to local", "err", fallbackErr)
	}

	merged := mergeDedup(local, fallback, beforeMS)
	if len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	return merged, nil
}

// entryCmp is the canonical (Time, UUID) ordering shared by the fast-path
// merge and the defensive sort so both paths order identical inputs alike.
func entryCmp(a, b clievent.EventEntry) int {
	if c := cmp.Compare(a.Time, b.Time); c != 0 {
		return c
	}
	return strings.Compare(a.UUID, b.UUID)
}

// contentKey is the cross-source identity that dedups a fallback entry
// against local when the tiers carry different UUIDs for the same turn.
// Keyed on (Type, Detail) — never Time (the tiers stamp one turn up to ~1.4 s
// apart; see skewWindowFor) and never Summary (local appends " [+N image(s)]"
// for attachments). Detail is normalised to the live tier's cap because the
// fallback readers truncate at the wider history.DetailMaxRunes. Returns ""
// (abstain; dedup by UUID only) when there is neither Detail nor Images.
// Image-only messages key on a length-prefixed SHA-256 of their thumbnails,
// which both tiers derive deterministically via cli.MakeThumbnail. A kind tag
// ("t"/"i") plus the 0x1f separator keep the encodings disjoint and unforgeable.
func contentKey(e clievent.EventEntry) string {
	if e.Detail == "" && len(e.Images) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(e.Type)
	b.WriteByte(0x1f)
	if e.Detail == "" {
		// Image-only identity.
		b.WriteString("i")
		b.WriteByte(0x1f)
		sum := sha256.New()
		var lenBuf [8]byte
		for _, img := range e.Images {
			binary.BigEndian.PutUint64(lenBuf[:], uint64(len(img)))
			sum.Write(lenBuf[:])
			sum.Write([]byte(img))
		}
		b.WriteString(hex.EncodeToString(sum.Sum(nil)))
		return b.String()
	}
	b.WriteString("t")
	b.WriteByte(0x1f)
	// Normalise to the live tier's cap; TruncateRunes is a no-op within the cap.
	b.WriteString(textutil.TruncateRunes(e.Detail, cli.EventDetailMaxRunes))
	return b.String()
}

// Skew bounds: how far apart the two tiers' timestamps for the SAME turn may
// sit and still be recognised as duplicates. The skew is directional and
// causal — whichever side RECEIVES the message stamps it later:
//
//	user          naozhi → CLI   local earlier   measured -1391..-8 ms
//	text/others   CLI → naozhi   local later     measured 0..+19 ms
//
// Both hosts share one clock, so this is pipeline latency, not drift. Sizing
// each bound for its own direction keeps assistant text from getting the 1.4 s
// window only cold-spawn user messages need.
const (
	// contentSkewLeadMS: how far local may lead (be older than) its twin — the
	// user direction. Measured ~1391 ms on a cold spawn; 3000 gives ~2x headroom.
	contentSkewLeadMS = 3000
	// contentSkewLagMS: how far local may lag (be newer) — the assistant
	// direction. Measured 19 ms; 500 stays far below the gap between real turns.
	contentSkewLagMS = 500
	// contentSkewEpsilonMS: slack AGAINST the causal direction for ms rounding.
	contentSkewEpsilonMS = 250
)

// skewWindowFor returns the inclusive (lead, lag) bounds for an entry Type:
// `lead` is how far local may be OLDER than its fallback twin, `lag` how far
// NEWER. Only "user" gets the naozhi→CLI orientation.
func skewWindowFor(entryType string) (lead, lag int64) {
	if entryType == "user" {
		return contentSkewLeadMS, contentSkewEpsilonMS
	}
	return contentSkewEpsilonMS, contentSkewLagMS
}

// dupSet holds the fallback indices pairContent decided are duplicates of a
// local entry. It is computed once for the whole page so the verdict is a
// fixed function of (local, fallback) — independent of walk order, which the
// fast and slow merge paths differ in — and one-to-one, so exactly min(N, M)
// same-content copies are ever removed.
type dupSet map[int]struct{}

// pairContent computes a maximum one-to-one pairing between local and
// fallback entries sharing a contentKey inside the directional skew window
// and returns the paired fallback indices. Only entries below the beforeMS
// cutoff take part; fallback entries whose UUID is in localUUIDs are excluded
// because the UUID branch already drops them and letting them consume a slot
// would double-charge the local row. Each bucket is solved by pairBucket,
// optimal on (1) maximum cardinality, then (2) minimum total |Δtime|. (1)
// bounds removals to min(N, M) so no tier loses a turn to over-collapsing;
// (2) decides which row survives so a Δ=0 twin never outlives a distinct
// turn beside it. Neither greedy alone delivers both.
func pairContent(local, fallback []clievent.EventEntry, beforeMS int64, localUUIDs map[string]struct{}) dupSet {
	visible := func(e clievent.EventEntry) bool {
		return beforeMS <= 0 || e.Time < beforeMS
	}

	// Bucket both sides by content key; buckets are sorted explicitly so the
	// pairing is independent of input order.
	type slot struct {
		idx  int
		time int64
	}
	localBuckets := make(map[string][]int64)
	for _, e := range local {
		if !visible(e) {
			continue
		}
		if k := contentKey(e); k != "" {
			localBuckets[k] = append(localBuckets[k], e.Time)
		}
	}
	if len(localBuckets) == 0 {
		return nil
	}
	fbBuckets := make(map[string][]slot)
	for i, e := range fallback {
		if !visible(e) {
			continue
		}
		// A fallback entry the UUID branch will drop must not consume a pairing
		// slot; otherwise a second same-content fallback record (e.g. the
		// rehashed uuid#N of a multi-block assistant line) would find nothing
		// to pair with and render.
		if e.UUID != "" {
			if _, dup := localUUIDs[e.UUID]; dup {
				continue
			}
		}
		if k := contentKey(e); k != "" {
			if _, ok := localBuckets[k]; ok {
				fbBuckets[k] = append(fbBuckets[k], slot{idx: i, time: e.Time})
			}
		}
	}
	if len(fbBuckets) == 0 {
		return nil
	}

	dups := make(dupSet)
	for k, fbSlots := range fbBuckets {
		localTimes := localBuckets[k]
		slices.Sort(localTimes)
		slices.SortFunc(fbSlots, func(a, b slot) int {
			if c := cmp.Compare(a.time, b.time); c != 0 {
				return c
			}
			return cmp.Compare(a.idx, b.idx)
		})
		// Every entry in a bucket shares a Type, so the window is uniform.
		lead, lag := skewWindowFor(typeOfKey(k))
		fbTimes := make([]int64, len(fbSlots))
		for i, fs := range fbSlots {
			fbTimes[i] = fs.time
		}
		for _, fbPos := range pairBucket(localTimes, fbTimes, lead, lag) {
			dups[fbSlots[fbPos].idx] = struct{}{}
		}
	}
	return dups
}

// pairBucket solves one content bucket: given ascending local and fallback
// Times sharing a key, it returns the fallback positions that pair with a
// local entry, maximising pair count first and total |Δtime| second. It is an
// O(N*M) dynamic program over order-preserving pairings (best[i][j] = optimal
// pairing of localTimes[i:] against fbTimes[j:]; moves: skip local, skip
// fallback, or pair when the skew is admissible). Order-preservation loses
// nothing: admissible partners form a contiguous Time range on each side, so
// crossing pairs can be uncrossed without changing count or cost. N and M are
// 1 for essentially all real content and bounded by the page limit anyway.
func pairBucket(localTimes, fbTimes []int64, lead, lag int64) []int {
	n, m := len(localTimes), len(fbTimes)
	if n == 0 || m == 0 {
		return nil
	}
	type cell struct {
		pairs int
		cost  int64
	}
	better := func(a, b cell) bool {
		if a.pairs != b.pairs {
			return a.pairs > b.pairs
		}
		return a.cost < b.cost
	}
	// dp over suffixes so reconstruction walks forward and emits ascending positions.
	dp := make([][]cell, n+1)
	for i := range dp {
		dp[i] = make([]cell, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			best := dp[i+1][j] // skip this local row
			if c := dp[i][j+1]; better(c, best) {
				best = c // skip this fallback row
			}
			if delta := localTimes[i] - fbTimes[j]; delta <= lag && delta >= -lead {
				if delta < 0 {
					delta = -delta
				}
				c := dp[i+1][j+1]
				c.pairs++
				c.cost += delta
				if better(c, best) {
					best = c
				}
			}
			dp[i][j] = best
		}
	}
	out := make([]int, 0, min(n, m))
	for i, j := 0, 0; i < n && j < m; {
		if delta := localTimes[i] - fbTimes[j]; delta <= lag && delta >= -lead {
			abs := delta
			if abs < 0 {
				abs = -abs
			}
			c := dp[i+1][j+1]
			c.pairs++
			c.cost += abs
			if c == dp[i][j] {
				out = append(out, j)
				i++
				j++
				continue
			}
		}
		if dp[i+1][j] == dp[i][j] {
			i++
			continue
		}
		j++
	}
	return out
}

// typeOfKey recovers the Type prefix of a contentKey; contentKey writes Type
// first followed by 0x1f, which cannot appear in a Type.
func typeOfKey(k string) string {
	if i := strings.IndexByte(k, 0x1f); i >= 0 {
		return k[:i]
	}
	return k
}

// mergeDedup merges two already-sorted slices with an O(n) two-way merge;
// when a source violates the chronological contract it falls back to
// concat+sort and logs a WARN.
//
// Dedup rule: on a shared UUID keep the LOCAL entry — it has full EventEntry
// fidelity (Images, AskQuestion, agent-team linkage…) where the JSONL
// fallback only has text. Entries without a UUID are never deduped by key:
// synthesising an identity risks collapsing unrelated events.
func mergeDedup(local, fallback []clievent.EventEntry, beforeMS int64) []clievent.EventEntry {
	if len(local) == 0 && len(fallback) == 0 {
		return nil
	}

	// Fast path: both inputs honour the chronological contract. isSortedContract
	// skips the scan for len ≤ 1, so the dominant single-tier shapes pay for
	// one O(n) scan (#2307).
	if isSortedContract(local) && isSortedContract(fallback) {
		return mergeSorted(local, fallback, beforeMS)
	}

	// Contract violation: warn so the offending Source.LoadBefore can be
	// traced, then repair with concat+sort.
	slog.Warn("merged: source returned unsorted entries; repairing with sort",
		"local_len", len(local), "fallback_len", len(fallback))
	return mergeSortFallback(local, fallback, beforeMS)
}

// isSortedContract reports whether s honours the (Time, UUID) contract that
// lets mergeDedup take the linear merge path. len ≤ 1 is trivially sorted and
// skips the scan — the common single-tier LoadBefore shape (#2307).
func isSortedContract(s []clievent.EventEntry) bool {
	if len(s) <= 1 {
		return true
	}
	return slices.IsSortedFunc(s, entryCmp)
}

// mergeSorted walks two sorted slices linearly and returns their deduped
// union, sorted by (Time, UUID). Local entries are ALWAYS kept; fallback
// entries are dropped when their UUID was already seen (from local or an
// earlier fallback entry) or when pairContent paired them. Empty UUIDs bypass
// UUID dedup.
func mergeSorted(local, fallback []clievent.EventEntry, beforeMS int64) []clievent.EventEntry {
	// fallback empty: `seen` is only ever read for fallback entries, so the
	// work reduces to local filtered by the cutoff. No map, no dedup.
	if len(fallback) == 0 {
		out := make([]clievent.EventEntry, 0, len(local))
		for _, e := range local {
			if beforeMS > 0 && e.Time >= beforeMS {
				continue
			}
			out = append(out, e)
		}
		return out
	}
	// local empty: only the fallback tail-flush runs. fallback is sorted by
	// (Time, UUID) and duplicate UUIDs share a Time, so they sort adjacently
	// and "drop later dups" reduces to "drop a UUID equal to the last non-empty
	// UUID emitted". Empty-UUID entries are kept and do NOT reset lastUUID, so
	// `b, <empty>, b` still drops the trailing b, matching the general path.
	if len(local) == 0 {
		out := make([]clievent.EventEntry, 0, len(fallback))
		lastUUID := ""
		for _, e := range fallback {
			if beforeMS > 0 && e.Time >= beforeMS {
				continue
			}
			if e.UUID != "" {
				if e.UUID == lastUUID {
					continue
				}
				lastUUID = e.UUID
			}
			out = append(out, e)
		}
		return out
	}

	// Step 1: seed `seen` with the UUID of every local entry the merge will
	// emit. A local entry above the cutoff must NOT seed `seen`, otherwise a
	// same-UUID fallback entry below the cutoff — a legitimately visible
	// backfill — would be deduped away.
	seen := make(map[string]struct{}, len(local))
	for _, e := range local {
		if beforeMS <= 0 || e.Time < beforeMS {
			if e.UUID != "" {
				seen[e.UUID] = struct{}{}
			}
		}
	}
	// contentDups pairs fallback entries whose UUID differs from their local
	// twin (see pairContent). Computed from the visible local set only, so it
	// can never collapse two distinct LOCAL events.
	contentDups := pairContent(local, fallback, beforeMS, seen)

	out := make([]clievent.EventEntry, 0, len(local)+len(fallback))
	emit := func(e clievent.EventEntry) {
		if beforeMS > 0 && e.Time >= beforeMS {
			return
		}
		out = append(out, e)
	}
	// fallbackDup: duplicate by UUID or by content pairing. Empty-UUID fallback
	// entries still get the content check. `seen` is extended at the emit
	// sites so a record read twice via a resume chain dedups against its
	// first copy; contentDups is immutable so the verdict cannot drift.
	fallbackDup := func(fi int, f clievent.EventEntry) bool {
		if f.UUID != "" {
			if _, dup := seen[f.UUID]; dup {
				return true
			}
		}
		_, dup := contentDups[fi]
		return dup
	}

	// Step 2: two-way merge. `<= 0` picks local on an exact (Time, UUID) tie
	// so "local wins" holds even for identical keys.
	i, j := 0, 0
	for i < len(local) && j < len(fallback) {
		if entryCmp(local[i], fallback[j]) <= 0 {
			emit(local[i])
			i++
			continue
		}
		f := fallback[j]
		dup := fallbackDup(j, f)
		// Record the UUID whether or not we emit: a resume chain can surface the
		// SAME fallback record twice, and if the content pairing removed the
		// first copy the second would otherwise leak.
		if f.UUID != "" {
			seen[f.UUID] = struct{}{}
		}
		if !dup {
			emit(f)
		}
		j++
	}
	for ; i < len(local); i++ {
		emit(local[i])
	}
	for ; j < len(fallback); j++ {
		f := fallback[j]
		dup := fallbackDup(j, f)
		if f.UUID != "" {
			seen[f.UUID] = struct{}{}
		}
		if dup {
			continue
		}
		emit(f)
	}
	return out
}

// mergeSortFallback is the concat-then-sort path for the contract-violation
// branch: seed `seen` from visible local entries and emit them, append
// non-duplicate fallback entries, then stable-sort by (Time, UUID). Kept
// separate from mergeDedup so the fast path's semantics stay obvious.
func mergeSortFallback(local, fallback []clievent.EventEntry, beforeMS int64) []clievent.EventEntry {
	seen := make(map[string]struct{}, len(local))
	out := make([]clievent.EventEntry, 0, len(local)+len(fallback))
	for _, e := range local {
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		if e.UUID != "" {
			seen[e.UUID] = struct{}{}
		}
		out = append(out, e)
	}
	// Same content pairing as the fast path. Computed after the local loop so
	// `seen` holds exactly the visible-local UUID set, before any fallback UUID
	// is recorded. pairContent sorts each bucket, so the verdict does not
	// depend on walk order.
	contentDups := pairContent(local, fallback, beforeMS, seen)
	for i, e := range fallback {
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		uuidDup := false
		if e.UUID != "" {
			_, uuidDup = seen[e.UUID]
			// Record whether or not we emit — mirrors mergeSorted.
			seen[e.UUID] = struct{}{}
		}
		if uuidDup {
			continue
		}
		if _, dup := contentDups[i]; dup {
			continue
		}
		out = append(out, e)
	}
	// Stable sort preserves local-first ordering for empty-UUID entries that
	// tie on (Time, UUID).
	slices.SortStableFunc(out, entryCmp)
	return out
}
