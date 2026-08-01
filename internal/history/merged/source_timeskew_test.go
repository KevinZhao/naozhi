package merged

import (
	"context"
	"strconv"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// Regression suite for the cross-tier clock-skew dedup failure.
//
// Symptom: every message in the dashboard rendered twice. Root cause: the
// cross-source dedup key embedded EventEntry.Time, but the two tiers stamp
// the same turn at different points in the pipeline — the local tier from
// cli.Event.recvAt (when readLoop pushed the frame onto eventCh), the Claude
// JSONL fallback from the CLI's own `timestamp` field. Measured within one
// live session that gap is 0-19 ms for assistant text and up to ~1.4 s for a
// user message on a cold CLI spawn. Since the two tiers' UUIDs also never
// coincide by construction (crypto/rand vs Claude's message uuid), BOTH dedup
// branches missed and the turn was emitted from each tier.
//
// The skew is directional (whoever receives stamps later), and the pairing is
// one-to-one, which together bound how much can ever be removed — see
// skewWindowFor / pairContent, TestMerged_SkewWindowIsDirectional and
// TestMerged_MoreFallbackThanLocal_ExcessSurvives.
//
// The fixtures below are real values lifted from the reproducing session
// (dashboard:direct:2026-07-31-214349-2-gaokao:general) so the tests pin the
// actual failure rather than a hand-tuned approximation.

// skewedPair builds the same logical turn as the two tiers saw it: identical
// Type/Summary/Detail, different UUID, and `skewMS` of pipeline skew applied
// in the CAUSAL direction for that entry type (see skewWindowFor):
//
//   - typ=="user"  — naozhi → CLI, so local is EARLIER than the fallback twin.
//   - otherwise    — CLI → naozhi, so local is LATER than the fallback twin.
func skewedPair(typ, text string, localTime, skewMS int64) (local, fallback cli.EventEntry) {
	fbTime := localTime - skewMS // local lags (assistant output)
	if typ == "user" {
		fbTime = localTime + skewMS // local leads (user message)
	}
	local = cli.EventEntry{
		UUID:    "bac2936f69e96fc17a532928505827a6",
		Time:    localTime,
		Type:    typ,
		Summary: text,
		Detail:  text,
	}
	fallback = cli.EventEntry{
		UUID:    "6a7f36b5a0994467ba9604ceaced1b7a",
		Time:    fbTime,
		Type:    typ,
		Summary: text,
		Detail:  text,
	}
	return local, fallback
}

// TestMerged_ClockSkewDoesNotDuplicate is the primary regression: the same
// turn seen by both tiers must render exactly once at every skew magnitude
// observed in production. Before the fix only skew==0 deduped, which is
// precisely why users saw "most messages doubled, a few not".
func TestMerged_ClockSkewDoesNotDuplicate(t *testing.T) {
	cases := []struct {
		typ   string
		text  string
		skews []int64
	}{
		// Assistant output (CLI → naozhi): measured 0-19 ms.
		{"text", "I'll start by looking at today's uncommitted work.",
			[]int64{0, 1, 2, 3, 19, contentSkewLagMS}},
		// User messages (naozhi → CLI): measured 8-1391 ms, the upper end on
		// a cold CLI spawn where naozhi stamps at stdin-write but the CLI
		// only records once its boot completes.
		{"user", "分析并整理归档",
			[]int64{0, 8, 10, 20, 462, 911, 1191, 1277, 1374, 1391, contentSkewLeadMS}},
	}
	for _, tc := range cases {
		for _, skew := range tc.skews {
			t.Run(tc.typ+"/skew="+strconv.FormatInt(skew, 10)+"ms", func(t *testing.T) {
				local, fallback := skewedPair(tc.typ, tc.text, 1785505503898, skew)
				m := &Source{
					Local:    &stubSource{entries: []cli.EventEntry{local}},
					Fallback: &stubSource{entries: []cli.EventEntry{fallback}},
				}
				got, err := m.LoadBefore(context.Background(), 0, 100)
				if err != nil {
					t.Fatalf("LoadBefore: %v", err)
				}
				if len(got) != 1 {
					t.Fatalf("%s skew=%dms rendered %d entries, want 1 (same turn duplicated)",
						tc.typ, skew, len(got))
				}
				if got[0].UUID != local.UUID {
					t.Errorf("local (richer) entry must win dedup, got UUID %q", got[0].UUID)
				}
			})
		}
	}
}

// TestMerged_ClockSkewBeyondTolerance_NotCollapsed is the other side of the
// contract: past contentSkewLeadMS the two entries are treated as
// distinct turns. Guards against a future widening of the window silently
// swallowing a genuinely repeated message sent minutes apart.
func TestMerged_ClockSkewBeyondTolerance_NotCollapsed(t *testing.T) {
	local, fallback := skewedPair("user", "继续", 1785508722370, contentSkewLeadMS+1)
	m := &Source{
		Local:    &stubSource{entries: []cli.EventEntry{local}},
		Fallback: &stubSource{entries: []cli.EventEntry{fallback}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (skew past tolerance = distinct turns)", len(got))
	}
}

// TestMerged_RepeatedSameTextTurns_CardinalityPreserved is the over-collapse
// guard. Dropping Time from the key without a bounded window would make every
// "继续" in a session collapse into one bubble. The gaokao session that
// exposed this bug genuinely contains two separate "继续" turns ~11 minutes
// apart, so this is the real-world shape, not a contrived edge case.
//
// Both tiers carry both turns → 2 in, 2 out (not 1).
func TestMerged_RepeatedSameTextTurns_CardinalityPreserved(t *testing.T) {
	const text = "继续"
	l1, f1 := skewedPair("user", text, 1785508722370, 1)
	l2, f2 := skewedPair("user", text, 1785509436985, 2) // ~11.9 min later
	// Distinct UUIDs per occurrence so UUID-dedup cannot mask a content bug.
	l2.UUID, f2.UUID = "aaaa0000000000000000000000000002", "bbbb0000000000000000000000000002"

	m := &Source{
		Local:    &stubSource{entries: []cli.EventEntry{l1, l2}},
		Fallback: &stubSource{entries: []cli.EventEntry{f1, f2}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (two genuine same-text turns must both render)", len(got))
	}
	for _, e := range got {
		if e.UUID != l1.UUID && e.UUID != l2.UUID {
			t.Errorf("local entries must win dedup, got UUID %q", e.UUID)
		}
	}
}

// TestMerged_FallbackOnlyRepeats_BothKept: when a range predates events/ so
// only the JSONL tier carries it, two same-text turns must still both render.
// The content pairing has a LOCAL row on one side by construction, so
// fallback-vs-fallback dedup stays UUID-only — this pins that asymmetry.
func TestMerged_FallbackOnlyRepeats_BothKept(t *testing.T) {
	m := &Source{
		Local: &stubSource{},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "f1", Time: 1000, Type: "user", Summary: "继续", Detail: "继续"},
			{UUID: "f2", Time: 2000, Type: "user", Summary: "继续", Detail: "继续"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (fallback-only repeats are distinct turns)", len(got))
	}
}

// TestMerged_MoreLocalThanFallback_ExtraLocalSurvives: local is always
// emitted verbatim (the package's core invariant), so 3 local copies + 1
// matching fallback copy yields exactly the 3 locals.
//
// The three same-text locals here mirror the real gaokao event log, where a
// separate send-path defect wrote the same user turn 4x. That is deliberate:
// the merge layer must not mask an upstream duplicate by silently collapsing
// local rows, because doing so would have hidden that defect entirely.
func TestMerged_MoreLocalThanFallback_ExtraLocalSurvives(t *testing.T) {
	const text = "分析并整理归档"
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "user", Summary: text, Detail: text}
	}
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			mk("l1", 1785505498634), mk("l2", 1785505498634), mk("l3", 1785505498634),
		}},
		// Within tolerance of all three locals (Δ=1191 ms — the observed
		// cold-spawn user-message skew).
		Fallback: &stubSource{entries: []cli.EventEntry{mk("f1", 1785505499825)}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 (3 locals emitted verbatim, fallback deduped)", len(got))
	}
	for _, e := range got {
		if e.UUID == "f1" {
			t.Error("fallback entry duplicated a local entry and should be dropped")
		}
	}
}

// TestMerged_FallbackOnlyTurnNotSwallowedByLocalTwin is the guard against the
// silent-history-loss regression that an earlier revision of this fix
// introduced. That revision made dedup STATEFUL: each local entry carried a
// consumable "credit", so a fallback-only turn with no local twin could claim
// an unrelated local entry's credit and vanish.
//
// The shape below is not a corner case — it is what every page looks like.
// LoadBefore passes the same limit to both tiers, but the local tier also
// stores thinking/tool_use/result rows the fallback never emits, so local's
// window always spans a SHORTER time range. There are therefore routinely
// fallback rows just below local's oldest entry.
//
// Here `fOld` is a genuinely distinct earlier turn that exists only in the
// fallback tier, and `lNew`/`fNew` are the two tiers' views of a later turn
// with the same text. Correct output: fOld + lNew. The stateful version
// emitted lNew + fNew — dropping a real message AND still double-rendering.
func TestMerged_FallbackOnlyTurnNotSwallowedByLocalTwin(t *testing.T) {
	const text = "继续"
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "user", Summary: text, Detail: text}
	}
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{mk("lNew", 5000)}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			mk("fOld", 3500), // distinct earlier turn, fallback-only
			mk("fNew", 5001), // same turn as lNew (1 ms skew)
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	var sawOld, sawTwin bool
	for _, e := range got {
		switch e.UUID {
		case "fOld":
			sawOld = true
		case "fNew":
			sawTwin = true
		}
	}
	if !sawOld {
		t.Error("fOld (a genuinely distinct fallback-only turn) was LOST")
	}
	if sawTwin {
		t.Error("fNew duplicates lNew and should have been deduped")
	}
	if len(got) != 2 {
		t.Errorf("got %d rows, want 2 (fOld + lNew)", len(got))
	}
}

// TestMerged_FallbackRecordReadTwice_DedupsAgainstFirstCopy: a resume chain
// can surface the SAME Claude record twice (identical UUID). Even when that
// record also duplicates a local entry by content, the second copy must not
// escape. The stateful revision leaked it, because the UUID branch burned the
// content credit and the next copy then found no credit to match.
func TestMerged_FallbackRecordReadTwice_DedupsAgainstFirstCopy(t *testing.T) {
	const text = "hello world"
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "text", Summary: text, Detail: text}
	}
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{mk("l1", 1000)}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			mk("f1", 1001), mk("f1", 1001), // same record twice
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (both fallback copies dup the local entry)", len(got))
	}
	if got[0].UUID != "l1" {
		t.Errorf("local should win, got %q", got[0].UUID)
	}
}

// TestMerged_ImageBearingUserTurnDedups: buildUserEntry appends
// " [+N image(s)]" to the LOCAL Summary (process_send.go) while the fallback
// tier emits the bare text (discovery/history_tail.go). A Summary-bearing
// content key therefore never matched, leaving the original doubling bug
// completely unfixed for every user message with an attachment — which is
// most of the traffic in the session that reported it. Detail is identical on
// both sides, which is why contentKey uses it instead.
func TestMerged_ImageBearingUserTurnDedups(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{{
			UUID: "localnative", Time: 5000, Type: "user",
			Summary: "看这个 [+1 image(s)]", Detail: "看这个",
			Images: []string{"data:image/jpeg;base64,A="},
		}}},
		Fallback: &stubSource{entries: []cli.EventEntry{{
			UUID: "claudeuuid", Time: 5200, Type: "user",
			Summary: "看这个", Detail: "看这个",
		}}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (image-bearing user turn still doubles)", len(got))
	}
	if got[0].UUID != "localnative" || len(got[0].Images) != 1 {
		t.Errorf("local (richer, has Images) must win, got %+v", got[0])
	}
}

// TestMerged_MoreFallbackThanLocal_ExcessSurvives is the M>N guard. The
// pairing is one-to-one, so exactly min(N,M) fallback copies are removed and
// every genuinely-distinct extra fallback turn still renders.
//
// A plain "does ANY local row match?" predicate fails this: it has no notion
// of how many, so all three fallback sends below collapse against local's
// single row and two real turns are silently lost.
func TestMerged_MoreFallbackThanLocal_ExcessSurvives(t *testing.T) {
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "user", Summary: "继续", Detail: "继续"}
	}
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{mk("lOne", 5000)}},
		// Three distinct sends, all inside the user lead window of local.
		Fallback: &stubSource{entries: []cli.EventEntry{
			mk("fA", 5500), mk("fB", 6500), mk("fC", 7500),
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 (only min(N,M)=1 copy may be removed)", len(got))
	}
	if got[0].UUID != "lOne" {
		t.Errorf("local must be emitted verbatim, got %q", got[0].UUID)
	}
}

// TestMerged_FallbackOnlyTurnOnCausalSide: a fallback-only turn sitting on the
// CAUSAL side of a local row (newer, for a user entry) is indistinguishable
// from a twin by direction alone. One-to-one pairing is what saves it: local's
// single row pairs with its nearest twin, leaving the distinct turn unpaired
// and therefore rendered.
func TestMerged_FallbackOnlyTurnOnCausalSide(t *testing.T) {
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "user", Summary: "继续", Detail: "继续"}
	}
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{mk("l", 10000)}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			mk("fTwin", 10001),     // the real twin (1 ms)
			mk("fDistinct", 11391), // a distinct turn, still inside lead=3000
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	var sawDistinct bool
	for _, e := range got {
		if e.UUID == "fDistinct" {
			sawDistinct = true
		}
	}
	if !sawDistinct {
		t.Error("fDistinct was LOST — a distinct turn on the causal side")
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (l + fDistinct)", len(got))
	}
}

// TestMerged_SkewWindowIsDirectional pins the causal orientation the window
// relies on: whichever side RECEIVES the message stamps it later.
//
//   - "user" flows naozhi → CLI, so local is EARLIER (leads) by up to ~1.4 s.
//     A fallback user entry that is OLDER than the local row cannot be its
//     twin, and must not be deduped — that anti-causal direction is exactly
//     how a fallback-only turn would get silently dropped.
//   - "text" flows CLI → naozhi, so local is LATER (lags) by ms only.
//
// Without the direction check, a symmetric window would accept both.
func TestMerged_SkewWindowIsDirectional(t *testing.T) {
	const text = "继续"
	mk := func(typ, uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: typ, Summary: text, Detail: text}
	}
	cases := []struct {
		name       string
		typ        string
		localTime  int64
		fbTime     int64
		wantMerged bool
	}{
		// user: local leads by 1391 ms — the real cold-spawn shape.
		{"user/causal-lead", "user", 5000, 6391, true},
		// user: fallback is 1391 ms OLDER than local. Anti-causal for a user
		// turn, so these are two distinct turns.
		{"user/anticausal-lag", "user", 5000, 3609, false},
		// text: local lags by 19 ms — the real assistant-output shape.
		{"text/causal-lag", "text", 5019, 5000, true},
		// text: fallback 2 s NEWER than local. Anti-causal for assistant
		// output, so distinct.
		{"text/anticausal-lead", "text", 5000, 7000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Source{
				Local:    &stubSource{entries: []cli.EventEntry{mk(tc.typ, "l1", tc.localTime)}},
				Fallback: &stubSource{entries: []cli.EventEntry{mk(tc.typ, "f1", tc.fbTime)}},
			}
			got, _ := m.LoadBefore(context.Background(), 0, 100)
			wantN := 2
			if tc.wantMerged {
				wantN = 1
			}
			if len(got) != wantN {
				t.Errorf("got %d rows, want %d (merged=%v)", len(got), wantN, tc.wantMerged)
			}
		})
	}
}

// TestMerged_DedupVerdictIsOrderIndependent pins the purity property that
// keeps the fast path (walks in (Time,UUID) order) and the slow path (walks
// in raw input order) in agreement: reversing the fallback input must not
// change the rendered set. A stateful, consume-as-you-go dedup fails this.
func TestMerged_DedupVerdictIsOrderIndependent(t *testing.T) {
	const text = "同一句话"
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "user", Summary: text, Detail: text}
	}
	local := []cli.EventEntry{mk("l1", 1000), mk("l2", 9000)}
	fwd := []cli.EventEntry{mk("f1", 1001), mk("f2", 5000), mk("f3", 9001)}
	rev := []cli.EventEntry{mk("f3", 9001), mk("f2", 5000), mk("f1", 1001)}

	count := func(fb []cli.EventEntry) int {
		m := &Source{Local: &stubSource{entries: local}, Fallback: &stubSource{entries: fb}}
		got, _ := m.LoadBefore(context.Background(), 0, 100)
		return len(got)
	}
	a, b := count(fwd), count(rev)
	if a != b {
		t.Errorf("dedup is order-dependent: forward=%d reversed=%d", a, b)
	}
	// f1 and f3 dup a local twin; f2 (4 s from either) is a distinct turn.
	if a != 3 {
		t.Errorf("got %d, want 3 (l1 + l2 + the distinct f2)", a)
	}
}

// TestMerged_ContentlessEntries_NotCrossMatched: an entry with no Detail has
// no comparable content, so contentKey must abstain ("") rather than bucket
// every such entry under its Type — otherwise two unrelated events would
// match each other and one would vanish. They remain UUID-deduped.
//
// Locally `result` is the genuinely content-less kind; the entries below use
// it rather than `thinking`, which does populate both fields
// (process_event_format.go). The fallback tier never emits either kind, so
// this guards a local-only shape.
func TestMerged_ContentlessEntries_NotCrossMatched(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "t1", Time: 1000, Type: "result"},
			{UUID: "t2", Time: 2000, Type: "result"},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "t3", Time: 3000, Type: "result"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 3 {
		t.Errorf("got %d, want 3 (content-less entries must not cross-match)", len(got))
	}
}

// TestMerged_ClockSkew_SlowPath: the same skew tolerance must hold on the
// defensive concat+sort branch. Descending input Time forces
// mergeSortFallback, so a fix applied only to mergeSorted would leave the
// duplicate visible whenever a source violates the sorted contract.
func TestMerged_ClockSkew_SlowPath(t *testing.T) {
	const a, b = "second turn", "first turn"
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "nativeB", Time: 2000, Type: "text", Summary: a, Detail: a},
			{UUID: "nativeA", Time: 1000, Type: "text", Summary: b, Detail: b},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "claudeB", Time: 1999, Type: "text", Summary: a, Detail: a}, // 1ms skew
			{UUID: "claudeA", Time: 981, Type: "text", Summary: b, Detail: b},  // 19ms skew
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (slow path must dedup across skew)", len(got))
	}
	for _, e := range got {
		if e.UUID != "nativeA" && e.UUID != "nativeB" {
			t.Errorf("local should win cross-source dedup, got UUID %q", e.UUID)
		}
	}
}

// TestMerged_TwoTiersTwoCopies_PairwiseMatched: two local copies and two
// fallback copies of the same text must pair up 1:1, leaving both locals and
// dropping both fallbacks. Passing the fallback entries in reverse order must
// not change the outcome — pairContent sorts each content bucket internally so
// the pairing is a fixed function of the inputs, which is what keeps the fast
// path (walks in (Time,UUID) order) and the slow path (raw input order) from
// disagreeing.
func TestMerged_TwoTiersTwoCopies_PairwiseMatched(t *testing.T) {
	const text = "ok"
	mk := func(uuid string, ts int64) cli.EventEntry {
		return cli.EventEntry{UUID: uuid, Time: ts, Type: "user", Summary: text, Detail: text}
	}
	local := []cli.EventEntry{mk("l1", 1000), mk("l2", 1900)}
	forward := []cli.EventEntry{mk("f1", 1002), mk("f2", 1905)}
	reversed := []cli.EventEntry{mk("f2", 1905), mk("f1", 1002)}

	for _, tc := range []struct {
		name string
		fb   []cli.EventEntry
	}{{"forward", forward}, {"reversed", reversed}} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Source{
				Local:    &stubSource{entries: local},
				Fallback: &stubSource{entries: tc.fb},
			}
			got, _ := m.LoadBefore(context.Background(), 0, 100)
			if len(got) != 2 {
				t.Fatalf("got %d, want 2 (2 locals kept, 2 fallbacks paired away)", len(got))
			}
			for _, e := range got {
				if e.UUID != "l1" && e.UUID != "l2" {
					t.Errorf("both locals should survive, got UUID %q", e.UUID)
				}
			}
		})
	}
}

// TestMerged_UUIDMatchAndDistinctLaterTurn: a fallback entry that dups local by
// exact UUID is dropped, and that must not cost a genuinely distinct same-text
// fallback turn further out. The distinct turn below sits far outside the skew
// window, so the pairing cannot reach it.
func TestMerged_UUIDMatchAndDistinctLaterTurn(t *testing.T) {
	const text = "重复文本"
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "shared", Time: 1000, Type: "user", Summary: text, Detail: text},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			// Same record read twice (exact UUID match) — dropped.
			{UUID: "shared", Time: 1000, Type: "user", Summary: text, Detail: text},
			// A genuinely different turn far outside the window — must survive.
			{UUID: "later", Time: 90000, Type: "user", Summary: text, Detail: text},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (UUID dup dropped, distinct later turn kept)", len(got))
	}
	var sawLater bool
	for _, e := range got {
		if e.UUID == "later" {
			sawLater = true
		}
	}
	if !sawLater {
		t.Error("distinct later fallback turn was wrongly absorbed")
	}
}

// TestMerged_ClockSkewRespectsBeforeMS: dedup must not let an entry the caller
// filtered out affect the result. A local entry at/above beforeMS is dropped
// by emit, so it must not seed the content pairing and swallow a visible
// fallback entry below the cutoff — the content-key analogue of
// TestMerged_AboveCutoffLocalDoesNotEvictFallback.
func TestMerged_ClockSkewRespectsBeforeMS(t *testing.T) {
	const text = "boundary"
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "localAbove", Time: 300, Type: "text", Summary: text, Detail: text},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "fallbackBelow", Time: 299, Type: "text", Summary: text, Detail: text},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 300, 100)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (visible fallback must survive an above-cutoff local)", len(got))
	}
	if got[0].UUID != "fallbackBelow" {
		t.Errorf("want fallbackBelow, got %q", got[0].UUID)
	}
}

// TestPairContent_RemovesExactlyMinNM is the cardinality contract expressed
// directly against pairContent, independent of how the merge paths consume it.
// With N local and M fallback copies of the same content all co-located in
// time (so every candidate pair is in-window), a one-to-one pairing must
// remove EXACTLY min(N, M) fallback entries — never more.
//
// This is the property that bounds both historical failure modes at once:
// removing more than min(N,M) loses real turns (the "any match" predicate did
// this when M>N); removing fewer leaves duplicates on screen.
func TestPairContent_RemovesExactlyMinNM(t *testing.T) {
	for _, typ := range []string{"user", "text"} {
		for n := 0; n <= 4; n++ {
			for m := 0; m <= 4; m++ {
				local := make([]cli.EventEntry, n)
				for i := range local {
					local[i] = cli.EventEntry{
						UUID: "l" + strconv.Itoa(i), Time: 10000, Type: typ, Detail: "s",
					}
				}
				fallback := make([]cli.EventEntry, m)
				for i := range fallback {
					fallback[i] = cli.EventEntry{
						UUID: "f" + strconv.Itoa(i), Time: 10000, Type: typ, Detail: "s",
					}
				}
				want := min(n, m)
				if got := len(pairContent(local, fallback, 0, nil)); got != want {
					t.Errorf("type=%s N=%d M=%d: removed %d, want min(N,M)=%d",
						typ, n, m, got, want)
				}
			}
		}
	}
}

// TestPairContent_WindowBoundary pins the inclusive edges of the directional
// window per Type: exactly at the bound pairs, one millisecond past does not.
// Guards against an off-by-one that would either re-open the duplicate or
// start swallowing turns just outside the measured skew.
func TestPairContent_WindowBoundary(t *testing.T) {
	for _, typ := range []string{"user", "text"} {
		lead, lag := skewWindowFor(typ)
		for _, tc := range []struct {
			delta int64 // local.Time - fallback.Time
			want  bool
		}{
			{lag, true}, {lag + 1, false},
			{-lead, true}, {-lead - 1, false},
		} {
			local := []cli.EventEntry{{UUID: "l", Time: 100000 + tc.delta, Type: typ, Detail: "d"}}
			fallback := []cli.EventEntry{{UUID: "f", Time: 100000, Type: typ, Detail: "d"}}
			paired := len(pairContent(local, fallback, 0, nil)) == 1
			if paired != tc.want {
				t.Errorf("type=%s delta=%+dms: paired=%v want=%v (lead=%d lag=%d)",
					typ, tc.delta, paired, tc.want, lead, lag)
			}
		}
	}
}

// TestPairContent_ClusteredDoesNotStrandPairs: when several same-content rows
// sit at different offsets inside one window, the greedy earliest-first sweep
// must still find the maximum pairing. If greedy could strand a pair, a real
// duplicate would survive on screen.
func TestPairContent_ClusteredDoesNotStrandPairs(t *testing.T) {
	const typ = "text"
	_, lag := skewWindowFor(typ)
	for shift := int64(0); shift <= lag; shift += 37 {
		local := []cli.EventEntry{
			{UUID: "l1", Time: 5000, Type: typ, Detail: "d"},
			{UUID: "l2", Time: 5000 + shift, Type: typ, Detail: "d"},
		}
		fallback := []cli.EventEntry{
			{UUID: "f1", Time: 5000 - lag, Type: typ, Detail: "d"},
			{UUID: "f2", Time: 5000, Type: typ, Detail: "d"},
		}
		if got := len(pairContent(local, fallback, 0, nil)); got != 2 {
			t.Errorf("shift=%dms: paired %d, want 2 (greedy stranded a pair)", shift, got)
		}
	}
}

// TestPairContent_PairsNearestNotEarliest is the assignment guard. Cardinality
// alone is not enough: with one local row and two in-window fallback rows, an
// earliest-first pairing removes whichever sorts first rather than the actual
// twin. Here the Δ=0 row is unmistakably the twin and the Δ=499 ms row is a
// distinct turn, so pairing the latter would drop a real message AND leave the
// twin rendering as the very duplicate this fix exists to remove.
func TestPairContent_PairsNearestNotEarliest(t *testing.T) {
	const text = "好的，已完成。"
	local := []cli.EventEntry{{UUID: "lRow", Time: 10000, Type: "text", Detail: text}}
	fallback := []cli.EventEntry{
		{UUID: "fDistinct", Time: 9501, Type: "text", Detail: text}, // Δ=499, distinct turn
		{UUID: "fTwin", Time: 10000, Type: "text", Detail: text},    // Δ=0, the real twin
	}
	dups := pairContent(local, fallback, 0, nil)
	if _, ok := dups[1]; !ok {
		t.Errorf("paired the farther candidate; want the Δ=0 twin at index 1, got %v", dups)
	}

	m := &Source{
		Local:    &stubSource{entries: local},
		Fallback: &stubSource{entries: fallback},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	var sawDistinct, sawTwin bool
	for _, e := range got {
		switch e.UUID {
		case "fDistinct":
			sawDistinct = true
		case "fTwin":
			sawTwin = true
		}
	}
	if !sawDistinct {
		t.Error("fDistinct (a real turn) was dropped instead of the twin")
	}
	if sawTwin {
		t.Error("the Δ=0 twin survived as a duplicate")
	}
}

// TestPairBucket_NearestFirstDoesNotStrandPairs is the counterpart: chasing the
// nearest pair must not cost cardinality. A plain nearest-first greedy takes
// (5000,5000) here — Δ=0 — and consumes both rows the second pair needed,
// stranding it and leaving a duplicate. Maximum cardinality comes first,
// minimum skew only breaks ties among maximum pairings.
func TestPairBucket_NearestFirstDoesNotStrandPairs(t *testing.T) {
	_, lag := skewWindowFor("text")
	localTimes := []int64{5000, 5000 + lag - 56}
	fbTimes := []int64{5000 - lag, 5000}
	if got := pairBucket(localTimes, fbTimes, contentSkewEpsilonMS, lag); len(got) != 2 {
		t.Errorf("paired %d, want 2 (nearest-first stranded a pair)", len(got))
	}
}

// TestPairContent_UUIDDroppedEntryKeepsItsSlot: a fallback entry that the UUID
// branch will drop anyway must not consume a local row's pairing slot. If it
// does, a SECOND same-content fallback record — a genuine duplicate, e.g. the
// rehashed uuid#N of a multi-block assistant line (discovery.uuidFromClaudeBlock)
// — finds nothing left to pair with and renders.
func TestPairContent_UUIDDroppedEntryKeepsItsSlot(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "X", Time: 1000, Type: "text", Detail: "d"},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "X", Time: 1000, Type: "text", Detail: "d"}, // dropped by UUID
			{UUID: "Y", Time: 1000, Type: "text", Detail: "d"}, // same turn, rehashed uuid
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (UUID-dropped entry must not burn the slot)", len(got))
	}
	if got[0].UUID != "X" {
		t.Errorf("local should win, got %q", got[0].UUID)
	}
}
