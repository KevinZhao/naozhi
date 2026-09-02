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
//   - Dedup. Two tiers can describe the SAME turn with DIFFERENT
//     UUIDs: the naozhi-native capture path stamps a crypto/rand
//     UUID (cli.newEventUUID) on assistant/tool_use/thinking events,
//     while the Claude JSONL fallback carries Claude's own message
//     uuid (discovery.uuidFromClaudeLine prefers hl.UUID). These two
//     identities never coincide, so a UUID-only dedup would render
//     the overlapping turn twice. MergedSource therefore dedups a
//     fallback entry against local on EITHER an exact UUID match OR a
//     matching content key (Type, Detail) whose timestamps sit inside a
//     directional skew window — see contentKey, pairContent and
//     skewWindowFor. UUID still wins where present (the legacy /
//     DeriveLegacyUUID case); the content key is the safety net for the
//     modern crypto-rand-vs-Claude-uuid steady state. The two tiers
//     stamp the same turn at different points in the pipeline, so the
//     content key must NOT include Time — doing so made the safety net
//     miss on every skewed turn and doubled every message in the
//     dashboard.
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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	case localErr != nil:
		// Single-source failure: the godoc promises we "log and degrade"
		// to the other tier. Without this an operator gets a silently
		// truncated response and no signal that the local tier failed.
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

// contentKey is the cross-source identity used to dedup a fallback
// entry against local when the two tiers carry different UUIDs for the
// same turn (native crypto/rand UUID vs Claude's own message uuid).
// Keyed on (Type, Detail).
//
// Time is deliberately NOT part of the key. The two tiers stamp the same
// turn at different points in the pipeline: the local tier uses
// cli.Event.recvAt (the moment readLoop pushed the frame onto eventCh)
// while the Claude JSONL fallback carries the CLI's own `timestamp`
// field. Measured within a single session those differ by 0-19 ms for
// assistant text and by up to ~1.4 s for a user message on a cold spawn
// (see skewWindowFor). So a Time-bearing key missed on EVERY skewed turn
// and — because the two tiers' UUIDs never coincide by construction
// (crypto/rand vs Claude's message uuid) — both dedup branches failed and
// the dashboard rendered the turn twice. Time still gates the match, but
// as a bounded window rather than an equality test — see pairContent.
//
// Summary is deliberately NOT part of the key either. For a user message
// carrying attachments, buildUserEntry appends " [+N image(s)]" to the
// LOCAL Summary (process_send.go), while the fallback tier emits the bare
// text (discovery/history_tail.go). Keying on Summary therefore missed
// every image-bearing turn — the original doubling bug, unfixed. Detail
// is the same text on both sides, so it is the reliable discriminator;
// it is also the wider field (2000/16000 runes vs Summary's 120), which
// makes an accidental cross-turn collision strictly less likely than the
// old Summary-bearing key allowed.
//
// Returns "" — meaning "no content identity, do not dedup by content" —
// when Detail is empty AND the entry carries no images. Such an entry has
// no comparable content at all, so a key over it would degenerate to one
// bucket per Type and let unrelated events match each other. Time was the
// only thing separating them before, and with Time out of the key the
// honest answer is to abstain. These entries are still deduped by exact
// UUID, the same conservative stance the package already takes for
// missing-UUID entries (see mergeDedup's "Missing UUID" note): never
// synthesize an identity we cannot defend. Locally a `result` event has no
// Detail; `thinking` does populate it.
//
// Image-only user messages are the exception that needs its own identity:
// both tiers emit them with Detail=="" (buildUserEntry truncates the empty
// text; discovery's parseHistoryLine likewise), so the Detail-based key
// abstained on BOTH sides and — the two tiers' UUIDs never coinciding by
// construction — every image-only message rendered twice, skewed apart by
// up to contentSkewLeadMS. The thumbnails ARE comparable content: both
// tiers derive them from the identical original bytes through the same
// deterministic pipeline (cli.MakeThumbnail at maxDim=600 — the local
// tier via buildUserEntry, the fallback via discovery.ThumbnailFn which
// claudejsonl's init wires to the same function), so the data URIs match
// byte-for-byte. Key on the thumbnails' digest; a decode failure that
// dropped a thumbnail on one side only makes the keys differ, i.e. we
// degrade to "no dedup" (a duplicate bubble) rather than ever collapsing
// two distinct messages.
//
// Two encoding details keep the key honest:
//
//   - A kind tag ("t" for text-keyed, "i" for image-keyed) puts the two
//     forms in disjoint namespaces. Without it the encodings overlap: a
//     Detail of "\x1f"+<image field> reproduces an image-keyed value
//     byte-for-byte. Contrived, but a separator alone cannot rule it out —
//     only a tag that no other branch can emit can.
//   - Thumbnails are digested rather than inlined. Each data URI runs
//     40-110 KB and contentKey is called per entry per tier on every
//     pairContent / LoadBefore pass, so inlining them allocated and copied
//     megabytes per merge to produce a value only ever used for equality.
//     A length-prefixed SHA-256 gives identical semantics at constant size;
//     the length prefix keeps the concatenation unambiguous so two
//     different image lists cannot digest to the same preimage.
//
// The 0x1f unit separator keeps field boundaries unambiguous so a value
// can't be forged by content that happens to contain the delimiter.
func contentKey(e cli.EventEntry) string {
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
	b.WriteString(e.Detail)
	return b.String()
}

// The skew bounds below decide how far apart the two tiers' timestamps
// for the SAME turn may sit and still be recognised as duplicates.
//
// The skew is DIRECTIONAL, and the direction is causal rather than
// incidental: whichever side RECEIVES the message stamps it later.
//
//	kind          producer → consumer     local-fallback   measured
//	------------  ----------------------  ---------------  ------------
//	user          naozhi   → CLI          negative         -1391..-8 ms
//	text/others   CLI      → naozhi       positive           0..+19 ms
//
// A user turn is stamped by buildUserEntry right after the stdin write,
// while the CLI only writes its JSONL record once it has read and booted
// far enough to process it — so local is always EARLIER, by up to ~1.4 s
// on a cold spawn. Assistant output runs the other way: the CLI stamps at
// generation time and naozhi stamps at readLoop receipt, so local is
// always LATER, by the transport latency alone. Both hosts share one
// clock (the CLI is naozhi's own child process), so this is pipeline
// latency, not clock drift.
//
// Exploiting the direction lets each bound be sized against the worst
// case measured FOR ITS OWN DIRECTION rather than shared. A single global
// tolerance would have to be as wide as the user-message case (1.4 s) and
// would then hand assistant text a 1.4 s window it never needs, widening
// the odds of pairing two unrelated same-text turns.
const (
	// contentSkewLeadMS bounds how far the LOCAL entry may lead (be older
	// than) its fallback twin — the user-message direction. Measured worst
	// case ~1391 ms on a cold spawn; 3000 gives ~2x headroom.
	contentSkewLeadMS = 3000
	// contentSkewLagMS bounds how far the LOCAL entry may lag (be newer
	// than) its fallback twin — the assistant-output direction. Measured
	// worst case 19 ms (transport latency only); 500 gives ~25x headroom
	// while staying far below the gap between two real turns.
	contentSkewLagMS = 500
	// contentSkewEpsilonMS is the slack allowed AGAINST the causal
	// direction, covering millisecond rounding when the two stamps land in
	// the same instant.
	contentSkewEpsilonMS = 250
)

// skewWindowFor returns the inclusive (lead, lag) bounds accepted for an
// entry of this Type, where `lead` is how far local may be OLDER than the
// fallback twin and `lag` how far it may be NEWER. Types other than "user"
// get the CLI→naozhi orientation; the fallback tier only ever emits "user"
// and "text", so that branch serves "text" in practice.
func skewWindowFor(entryType string) (lead, lag int64) {
	if entryType == "user" {
		// naozhi → CLI: local leads.
		return contentSkewLeadMS, contentSkewEpsilonMS
	}
	// CLI → naozhi: local lags.
	return contentSkewEpsilonMS, contentSkewLagMS
}

// dupSet is the set of fallback entries (by index into the fallback slice)
// that pairContent decided are duplicates of a local entry. Membership is
// computed ONCE, up front, for the whole page.
//
// Why precompute instead of deciding per entry as the merge walks:
//
//   - A stateful "consume a credit as you go" decision is order-dependent,
//     so the fast path (walks in (Time,UUID) order) and the slow path
//     (walks in raw input order) reached different verdicts on the same
//     logical input.
//   - A pure "does ANY local row match?" predicate is order-independent but
//     has no notion of how many: when the fallback tier holds MORE
//     same-content copies than local does, every excess copy is dropped
//     against local's single row, silently losing real turns.
//
// Deciding the whole pairing before either path runs gives both properties
// at once: the verdict is a fixed function of (local, fallback), and it is
// one-to-one, so exactly min(N, M) copies are ever removed.
type dupSet map[int]struct{}

// pairContent computes a maximum one-to-one pairing between local and
// fallback entries that share a contentKey and sit inside the directional
// skew window, and returns the set of fallback indices that got paired.
//
// Only fallback indices below the beforeMS cutoff are considered, and only
// local entries below it seed the pairing — a local row the caller filtered
// out must not absorb a fallback row that is legitimately visible.
//
// localUUIDs is the set of UUIDs carried by those visible local entries.
// Fallback entries matching one are excluded from the pairing entirely: the
// UUID branch in the merge already drops them, so letting them consume a
// pairing slot would double-charge the local row.
//
// Within a content bucket the pairing is resolved by a small dynamic program
// (pairBucket) that is optimal on BOTH criteria, in strict priority order:
//
//  1. Maximum cardinality — pair as many rows as possible.
//  2. Among all maximum pairings, minimum total |Δtime|.
//
// Both matter, and a single greedy cannot deliver both:
//
//   - Earliest-first greedy gets (1) but not (2). Given one local row and two
//     in-window fallback rows — a Δ=0 twin and a Δ=499 ms distinct turn — it
//     pairs the DISTINCT turn because that sorts first, dropping a real
//     message while the unmistakable twin renders as exactly the duplicate
//     this fix exists to remove.
//   - Nearest-first greedy gets (2) but not (1). Locals at 5000/5444 against
//     fallbacks at 4500/5000 has a 2-pair solution, but taking the Δ=0 pair
//     (5000,5000) first consumes both rows the other pair needed and strands
//     it, leaving a duplicate on screen.
//
// Criterion (1) bounds HOW MANY rows can be removed (exactly min(N,M), so
// neither tier can lose a turn to over-collapsing); criterion (2) decides
// WHICH row survives, which is what keeps a Δ=0 twin from outliving the
// distinct turn beside it.
//
// The one-to-one property is what bounds the damage in both directions:
//
//   - N local, M fallback copies => exactly min(N, M) fallback copies are
//     dropped, so max(0, M-N) genuinely-distinct fallback turns survive
//     and local's N rows are always emitted verbatim.
//   - An unpaired local row cannot absorb anything, so a fallback-only turn
//     is only ever dropped when it truly has an unpaired same-content twin
//     inside the window.
func pairContent(local, fallback []cli.EventEntry, beforeMS int64, localUUIDs map[string]struct{}) dupSet {
	visible := func(e cli.EventEntry) bool {
		return beforeMS <= 0 || e.Time < beforeMS
	}

	// Bucket both sides by content key. Times stay in input order; callers
	// hand us Time-ascending slices on the fast path, and the slow path is
	// only reached when a source violated that contract, so we sort each
	// bucket explicitly to make the pairing independent of input order.
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
		// A fallback entry that an exact UUID match will drop anyway must not
		// consume a local row's pairing slot: the slot would be spent on a row
		// that was already going to be removed, and a SECOND same-content
		// fallback record — a genuine duplicate, e.g. the rehashed uuid#N of a
		// multi-block assistant line — would then find nothing to pair with
		// and render. Skipping them here keeps the two dedup mechanisms from
		// double-charging the same local row.
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
		// Every entry in a bucket shares the same Type (it is part of the
		// key), so the window is uniform across the bucket.
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

// pairBucket solves one content bucket: given the Times of the local and
// fallback entries sharing a content key (both ascending), it returns the
// fallback positions that pair with a local entry.
//
// The pairing maximises the number of pairs first and minimises the total
// |Δtime| second (see pairContent for why both are required). It is computed
// by an O(N*M) dynamic program over order-preserving pairings:
//
//	best[i][j] = the optimal pairing of localTimes[i:] against fbTimes[j:]
//
// with three moves at each step — skip the local row, skip the fallback row,
// or pair them when the skew is admissible. Restricting to order-preserving
// pairings loses nothing: admissible partners form a contiguous Time range on
// each side, so any crossing pair can be uncrossed without reducing the count
// or increasing the total skew.
//
// N and M are 1 for essentially all real content (a distinct sentence appears
// once in a page), so the quadratic term is nominal; both are bounded by the
// caller's page limit regardless (visibleDiskPageSize=200).
func pairBucket(localTimes, fbTimes []int64, lead, lag int64) []int {
	n, m := len(localTimes), len(fbTimes)
	if n == 0 || m == 0 {
		return nil
	}
	// pairs is negated in the comparison below so that a single "smaller is
	// better" ordering covers "more pairs, then less skew".
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
	// dp[i][j] over the suffixes, so the reconstruction below walks forward
	// and emits fallback positions in ascending order.
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

// typeOfKey recovers the Type prefix a contentKey was built from. Cheaper
// than threading the Type alongside every bucket, and unambiguous because
// contentKey writes Type first followed by the 0x1f separator, which
// cannot appear in a Type.
func typeOfKey(k string) string {
	if i := strings.IndexByte(k, 0x1f); i >= 0 {
		return k[:i]
	}
	return k
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
// discovery's textutil.DeriveLegacyUUID together ensure this case is rare —
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
	//
	// R202606g-PERF-001 (#2307): the dominant LoadBefore shapes are
	// single-tier (events/ empty so only fallback carries rows, or
	// Claude JSONL absent so only local does). isSortedContract skips
	// the scan entirely for a slice of length ≤1 (trivially sorted),
	// so the single-tier and empty-tier cases pay for exactly one
	// O(n) scan instead of two — without weakening the correctness
	// fallback for the genuinely-unsorted case.
	if isSortedContract(local) && isSortedContract(fallback) {
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

// isSortedContract reports whether s honours the (Time, UUID) chronological
// contract that lets mergeDedup take the linear merge path. A slice of length
// ≤1 is trivially sorted and skips the scan outright — this is the common
// single-tier LoadBefore shape (one source empty, the other carrying the page)
// where forcing the second IsSortedFunc walk was pure overhead (R202606g-PERF-001,
// #2307). For len > 1 it delegates to slices.IsSortedFunc so the defensive
// concat+sort fallback still fires on a genuine contract violation.
func isSortedContract(s []cli.EventEntry) bool {
	if len(s) <= 1 {
		return true
	}
	return slices.IsSortedFunc(s, entryCmp)
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
	// Fast paths: when only one tier carries data (the common upgrade
	// case — events/ empty so fallback fills the gap, or Claude JSONL
	// absent so only local has rows) the dedup `seen` map is pure waste:
	// it's only ever READ for fallback entries against local-seeded keys,
	// so with one side empty the map is built and never consulted (or only
	// consulted for the surviving tier's self-dedup, which we inline).
	//
	// fallback empty: `seen` would be seeded from local but the only reads
	// of `seen` happen in the two fallback loops, both of which are no-ops
	// when len(fallback)==0. The remaining work is exactly emit(local[i])
	// for every i in order — i.e. local filtered by emit's cutoff. No map,
	// no dedup. Element-for-element identical to the general path.
	if len(fallback) == 0 {
		out := make([]cli.EventEntry, 0, len(local))
		for _, e := range local {
			if beforeMS > 0 && e.Time >= beforeMS {
				continue
			}
			out = append(out, e)
		}
		return out
	}
	// local empty: step-1 seeding produces an empty `seen`, the two-way
	// merge loop never runs (no local), and only the fallback tail-flush
	// executes. That flush is: keep empty-UUID entries unconditionally;
	// for non-empty UUIDs, keep the FIRST occurrence and drop later dups;
	// all subject to emit's cutoff. Because fallback is sorted by
	// (Time, UUID) — and a stable UUID is per-turn so duplicate UUIDs share
	// the same Time and therefore sort adjacently — "drop later dups"
	// reduces to "drop a non-empty UUID equal to the last non-empty UUID
	// emitted". A single lastUUID tracker replaces the dedup map entirely,
	// so this single-tier fast path allocates only the result slice.
	//
	// Empty UUIDs are always kept (they bypass the dedup branch) and do NOT
	// clear lastUUID: leaving the tracker intact across an empty-UUID entry
	// means a `b, <empty>, b` run still drops the trailing `b`, matching the
	// reference's global-set "keep first occurrence" rule element-for-element.
	if len(local) == 0 {
		out := make([]cli.EventEntry, 0, len(fallback))
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

	// Step 1: seed `seen` with the UUID of every local entry that the
	// merge will actually emit so fallback entries are checked against
	// the full *visible* local set, not just those already emitted. The
	// `beforeMS <= 0 || e.Time < beforeMS` guard mirrors emit's cutoff
	// (line below): a local entry that emit would drop (Time >= beforeMS)
	// must NOT seed `seen`, otherwise a same-UUID fallback entry that is
	// below the cutoff — and therefore a legitimately visible backfill —
	// would be silently deduped away. Sizing at len(local) is a safe
	// upper bound — guarded-out and empty-UUID entries simply won't
	// populate a slot, but over-allocating a few map buckets is cheaper
	// than a rehash when the map grows during the fallback tail flush.
	seen := make(map[string]struct{}, len(local))
	for _, e := range local {
		if beforeMS <= 0 || e.Time < beforeMS {
			if e.UUID != "" {
				seen[e.UUID] = struct{}{}
			}
		}
	}
	// contentDups is the precomputed one-to-one content pairing (see
	// pairContent). It recognises a fallback entry whose UUID differs from
	// its local twin — the native-crypto-rand vs Claude-uuid steady state —
	// even though the two tiers stamp the turn milliseconds apart. Computed
	// from the visible local set only, so it can never collapse two distinct
	// LOCAL events; those are always emitted via the local branch.
	contentDups := pairContent(local, fallback, beforeMS, seen)

	out := make([]cli.EventEntry, 0, len(local)+len(fallback))
	emit := func(e cli.EventEntry) {
		if beforeMS > 0 && e.Time >= beforeMS {
			return
		}
		out = append(out, e)
	}
	// fallbackDup reports whether the fallback entry at index fi duplicates
	// a local entry — by UUID, or by having been paired in contentDups.
	// Empty-UUID fallback entries still get the content check so a
	// legacy-derived fallback row and its native local twin don't both
	// render.
	//
	// `seen` is extended at the emit sites below so a fallback record read
	// twice via a resume chain dedups against its first copy. contentDups
	// is immutable, so the content verdict is a fixed function of the
	// inputs and cannot drift with walk order.
	fallbackDup := func(fi int, f cli.EventEntry) bool {
		if f.UUID != "" {
			if _, dup := seen[f.UUID]; dup {
				return true
			}
		}
		_, dup := contentDups[fi]
		return dup
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
		dup := fallbackDup(j, f)
		// Record the UUID whether or not we emit: a resume chain can surface
		// the SAME fallback record twice, and if the first copy was removed
		// by the content pairing (which consumed local's only credit) the
		// second copy would otherwise find nothing to dedup against and leak.
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
	// contentDups: cross-source content pairing, IDENTICAL semantics to the
	// fast path. Computed AFTER the local loop so `seen` already holds exactly
	// the visible-local UUID set that pairContent needs, and before any
	// fallback UUID is recorded into it. pairContent sorts each content bucket
	// internally, so the verdict does not depend on the order this path walks
	// the input — that is what keeps the two paths in agreement even though
	// this one is only reached when a source violated the sorted contract.
	contentDups := pairContent(local, fallback, beforeMS, seen)
	for i, e := range fallback {
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		uuidDup := false
		if e.UUID != "" {
			_, uuidDup = seen[e.UUID]
			// Record whether or not we emit — mirrors mergeSorted, so a
			// repeated record whose first copy was removed by the content
			// pairing cannot leak through as a second bubble.
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
	// SortStableFunc preserves local-first ordering for entries that
	// tie on (Time, UUID) — matching the original sort.SliceStable
	// contract. Needed for empty-UUID entries that could legitimately
	// tie; non-empty-UUID ties are already resolved by the dedup pass.
	slices.SortStableFunc(out, entryCmp)
	return out
}
