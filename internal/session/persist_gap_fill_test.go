package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/schema"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// gapSrc is an in-memory history.Source over entries sorted by Time:
// LoadBefore returns a copy of the newest `limit` with Time < before.
type gapSrc struct{ es []clievent.EventEntry }

func (s gapSrc) LoadBefore(_ context.Context, before int64, limit int) ([]clievent.EventEntry, error) {
	end := len(s.es)
	if before > 0 {
		end, _ = slices.BinarySearchFunc(s.es, before, func(e clievent.EventEntry, t int64) int { return cmp.Compare(e.Time, t) })
	}
	return slices.Clone(s.es[max(0, end-limit):end]), nil
}

// gapCase is one generated session: the local tier as persist would have
// written it (dropped batches removed, a gap record in front of the next
// stored entry), the fallback tier (twins of every user/text turn), and the
// local user/text turns that were dropped.
type gapCase struct {
	local, fallback []clievent.EventEntry
	drops           []clievent.EventEntry // every dropped user/text turn
	markedDrops     []clievent.EventEntry // those with a gap record after them
	twinOf          map[string]string     // dropped local UUID -> fallback twin UUID
}

// gapLocalOnly are local entry types the fallback tier never carries.
var gapLocalOnly = []string{"thinking", "tool_use", "tool_result", "task_progress", "system", "ask_question"}

// genGapCase builds turns of: a user message (1/15 image-only, 1/6 a repeated
// "ok"; fallback twin stamped 8..1391 ms later), 0..7 local-only entries, and
// 1..2 assistant texts (1/8 a repeated "done"; twin up to 19 ms earlier).
// burstP percent of steps share the previous millisecond. 1..3 runs of 1..12
// records are dropped; a run with a stored entry after it is marked by a gap
// record, a run at the tail is not (persist never writes that record).
func genGapCase(rnd *rand.Rand, turns, burstP int) gapCase {
	type rec struct {
		l    clievent.EventEntry
		twin *clievent.EventEntry
	}
	var recs []rec
	id := 0
	nid := func(p string) string { id++; return fmt.Sprintf("%s%x-%d", p, rnd.Int63(), id) }
	step := func(t int64) int64 {
		if rnd.Intn(100) < burstP {
			return t
		}
		return t + int64(1+rnd.Intn(3000))
	}
	t := int64(1_000_000)
	for i := 0; i < turns; i++ {
		t += int64(1500 + rnd.Intn(8000))
		u := clievent.EventEntry{UUID: nid("l"), Time: t, Type: "user", Detail: fmt.Sprintf("u%d", i)}
		if rnd.Intn(6) == 0 {
			u.Detail = "ok"
		}
		if rnd.Intn(15) == 0 {
			u.Detail, u.Images = "", []string{fmt.Sprintf("img%d", rnd.Intn(3))}
		}
		fu := u
		fu.UUID, fu.Time = nid("c"), t+int64(8+rnd.Intn(1384))
		recs = append(recs, rec{l: u, twin: &fu})
		for k, n := 0, rnd.Intn(8); k < n; k++ {
			t = step(t)
			typ := gapLocalOnly[rnd.Intn(len(gapLocalOnly))]
			recs = append(recs, rec{l: clievent.EventEntry{UUID: nid("l"), Time: t, Type: typ, Detail: fmt.Sprintf("%s%d", typ, rnd.Intn(4))}})
		}
		t = step(t)
		for b, n := 0, 1+rnd.Intn(2); b < n; b++ {
			a := clievent.EventEntry{UUID: nid("l"), Time: t, Type: "text", Detail: fmt.Sprintf("a%d.%d", i, b)}
			if rnd.Intn(8) == 0 {
				a.Detail = "done"
			}
			fa := a
			fa.UUID, fa.Time = nid("c"), t-int64(rnd.Intn(20))
			recs = append(recs, rec{l: a, twin: &fa})
		}
	}
	drop := make([]bool, len(recs))
	n := len(recs)
	for k, runs := 0, 1+rnd.Intn(3); k < runs; k++ {
		from := n/4 + rnd.Intn(n-n/4-1)
		for i := from; i < from+1+rnd.Intn(12) && i < n; i++ {
			drop[i] = true
		}
	}
	c := gapCase{twinOf: map[string]string{}}
	var pending []clievent.EventEntry
	dropping := false
	for i, r := range recs {
		if r.twin != nil {
			c.fallback = append(c.fallback, *r.twin)
		}
		if drop[i] {
			dropping = true
			if r.twin != nil {
				pending = append(pending, r.l)
				c.drops = append(c.drops, r.l)
				c.twinOf[r.l.UUID] = r.twin.UUID
			}
			continue
		}
		if dropping {
			c.local = append(c.local, clievent.EventEntry{Time: r.l.Time, Type: schema.GapEntryType, Detail: "dropped"})
			c.markedDrops = append(c.markedDrops, pending...)
			pending, dropping = nil, false
		}
		c.local = append(c.local, r.l)
	}
	slices.SortStableFunc(c.fallback, func(a, b clievent.EventEntry) int { return cmp.Compare(a.Time, b.Time) })
	return c
}

// walkReachable pages a session the way its readers do and counts how often
// each UUID is served: the initial page, then load-earlier pages cursored on
// the previous page's first entry. frontend stops where dashboard.js does
// (hasMore false, or a page shorter than the limit); otherwise the walk goes
// until the history is exhausted.
func walkReachable(s *ManagedSession, pageLimit int, frontend bool) (map[string]int, []clievent.EventEntry) {
	ctx := context.Background()
	seen := map[string]int{}
	var served []clievent.EventEntry
	take := func(page []clievent.EventEntry) {
		for _, e := range page {
			seen[e.UUID]++
			served = append(served, e)
		}
	}
	page, hasMore := s.EventInitialPageCtx(ctx, DefaultVisibleTarget, 0)
	take(page)
	if len(page) == 0 || (frontend && !hasMore) {
		return seen, served
	}
	before := page[0].Time
	for range 10_000 {
		older := s.EventEntriesBeforeCtx(ctx, before, pageLimit)
		if len(older) == 0 {
			break
		}
		take(older)
		if (frontend && len(older) < pageLimit) || older[0].Time >= before {
			break
		}
		before = older[0].Time
	}
	return seen, served
}

// gapLeadMS mirrors merged's contentSkewLeadMS, the upper widening of a gap
// window.
const gapLeadMS = 3000

func gapContentKey(e clievent.EventEntry) string {
	return fmt.Sprintf("%s\x1f%s\x1f%v", e.Type, e.Detail, e.Images)
}

// TestPersistGapFill_NeverFewer is the S15 acceptance property, on the real
// ManagedSession read path. For every generated session, "old" is today's
// state (tier 1 injects the newest `limit` local entries) and "new" also runs
// fillPersistGaps over a 2*limit look-back, as tier 1 does. Both are walked
// server-style and frontend-style. Asserted per case:
//  1. every local UUID old serves, new serves too (local entries never lost);
//  2. new serves no UUID twice, and no user/text content inside the memory
//     window more often than old did plus the drops of that content a gap
//     window can recover: marked ones, and an unmarked tail drop within the
//     lead bound of the last gap record (no twin leaks);
//  3. a marked-dropped user/text turn inside the memory window (3 s clear of
//     its floor) is served by content.
//
// Unmarked tail drops are out of scope: persist never records them.
func TestPersistGapFill_NeverFewer(t *testing.T) {
	t.Parallel()
	for _, cfg := range []struct {
		name          string
		turns, burstP int
		limit, page   int
		frontend      bool
	}{
		{"small-server", 40, 30, 60, 17, false},
		{"small-frontend", 40, 30, 60, 17, true},
		{"prod-server", 200, 30, maxPersistedHistory, 100, false},
		{"prod-frontend", 200, 30, maxPersistedHistory, 100, true},
		{"prod-burst-server", 200, 70, maxPersistedHistory, 100, false},
		{"prod-burst-frontend", 200, 70, maxPersistedHistory, 100, true},
	} {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()
			rnd := rand.New(rand.NewSource(7))
			ctx := context.Background()
			var gapTurns, servedOld, servedNew, filledCases int
			for ci := range 300 {
				c := genGapCase(rnd, cfg.turns/2+rnd.Intn(cfg.turns), cfg.burstP)
				src := &merged.Source{Local: gapSrc{c.local}, Fallback: gapSrc{c.fallback}}
				tail, _ := gapSrc{c.local}.LoadBefore(ctx, 0, cfg.limit)
				lookBack, _ := gapSrc{c.local}.LoadBefore(ctx, 0, 2*cfg.limit)
				old := &ManagedSession{key: "k"}
				old.SetHistorySource(src)
				old.InjectHistoryIfEmpty(tail)
				nw := &ManagedSession{key: "k"}
				nw.SetHistorySource(src)
				nw.InjectHistoryIfEmpty(tail)
				nw.fillPersistGaps(ctx, lookBack, tail[0].Time)
				if nw.loadGapFill() != nil {
					filledCases++
				}
				ro, _ := walkReachable(old, cfg.page, cfg.frontend)
				rn, servedN := walkReachable(nw, cfg.page, cfg.frontend)

				for _, e := range c.local {
					if e.UUID != "" && ro[e.UUID] > 0 && rn[e.UUID] == 0 {
						t.Fatalf("case %d: local %s %s@%d served before, not after", ci, e.UUID, e.Type, e.Time)
					}
				}
				for u, n := range rn {
					if u != "" && n > 1 {
						t.Fatalf("case %d: UUID %s served %d times", ci, u, n)
					}
				}

				floor := tail[0].Time
				turnInWindow := func(e clievent.EventEntry, from int64) bool {
					return (e.Type == "user" || e.Type == "text") && e.Time >= from
				}
				byUUID := map[string]clievent.EventEntry{}
				for _, e := range c.local {
					byUUID[e.UUID] = e
				}
				for _, e := range c.fallback {
					byUUID[e.UUID] = e
				}
				contentCount := func(reach map[string]int) map[string]int {
					out := map[string]int{}
					for u := range reach {
						if e, ok := byUUID[u]; ok && turnInWindow(e, floor) {
							out[gapContentKey(e)]++
						}
					}
					return out
				}
				co, cn := contentCount(ro), contentCount(rn)
				// A fill may serve the twin of a marked drop, or of an
				// unmarked tail drop whose twin is within the lead bound of
				// the last gap record; drops elsewhere do not excuse a twin.
				lastGap := int64(-1 << 62)
				for _, e := range c.local {
					if e.Type == schema.GapEntryType {
						lastGap = e.Time
					}
				}
				marked := map[string]bool{}
				for _, d := range c.markedDrops {
					marked[d.UUID] = true
				}
				dropped := map[string]int{}
				for _, d := range c.drops {
					tw := byUUID[c.twinOf[d.UUID]]
					if tw.Time >= floor && (marked[d.UUID] || tw.Time <= lastGap+gapLeadMS) {
						dropped[gapContentKey(d)]++
					}
				}
				for k, n := range cn {
					if n > co[k]+dropped[k] {
						t.Fatalf("case %d: content %q served %d times, old %d + dropped %d: a twin leaked", ci, k, n, co[k], dropped[k])
					}
				}
				want := map[string]int{}
				for _, e := range c.local {
					if e.UUID != "" && ro[e.UUID] > 0 && turnInWindow(e, floor+3000) {
						want[gapContentKey(e)]++
					}
				}
				for _, d := range c.markedDrops {
					if turnInWindow(d, floor+3000) {
						want[gapContentKey(d)]++
					}
				}
				got := map[string]int{}
				for _, e := range servedN {
					if turnInWindow(e, floor) {
						got[gapContentKey(e)]++
					}
				}
				for k, n := range want {
					if got[k] < n {
						t.Fatalf("case %d: content %q served %d times, want %d (a marked drop was not filled)", ci, k, got[k], n)
					}
				}
				for _, d := range c.markedDrops {
					if d.Time < floor {
						continue
					}
					gapTurns++
					if ro[c.twinOf[d.UUID]] > 0 {
						servedOld++
					}
					if rn[c.twinOf[d.UUID]] > 0 {
						servedNew++
					}
				}
			}
			if filledCases == 0 {
				t.Fatal("no case produced a gap fill: the generator no longer exercises the path")
			}
			t.Logf("cases=300 filled=%d markedDropsInMemoryWindow=%d twinServedOld=%d twinServedNew=%d",
				filledCases, gapTurns, servedOld, servedNew)
		})
	}
}

// blockingFallback releases LoadBefore only when release is closed.
type blockingFallback struct {
	release chan struct{}
	entries []clievent.EventEntry
}

func (b *blockingFallback) LoadBefore(ctx context.Context, before int64, limit int) ([]clievent.EventEntry, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return gapSrc{b.entries}.LoadBefore(ctx, before, limit)
}

// gapLoaderRouter writes records to a real event log for key and returns a
// Router holding one session for key whose fallback tier is fb, ready for
// startBackgroundHistoryLoaders. claudeDir stays empty so tier 2 does not run.
func gapLoaderRouter(t *testing.T, key string, records []string, times []int64, fb *blockingFallback) (*Router, *ManagedSession) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "events")
	p, err := persist.NewPersister(persist.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	sink := p.SinkFor(key)
	for i, js := range records {
		sink([]persist.Entry{{JSON: []byte(js), TimeMS: times[i]}}, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	hctx, hcancel := context.WithCancel(context.Background())
	t.Cleanup(hcancel)
	r := &Router{ss: newSessionTable(), hist: HistoryIO{ctx: hctx, persister: p, eventLogDir: dir}}
	s := &ManagedSession{key: key}
	s.SetHistorySource(&merged.Source{Local: newEventLogLocalSource(dir, key), Fallback: fb})
	r.ss.Update(func(tx sessTx) { tx.Put(key, s) })
	return r, s
}

// TestPersistGapFill_InjectDoesNotWait: tier 1 injects the event-log tail
// before it reads the fallback, so a slow transcript cannot hand the startup
// race to another loader. With the fallback blocked the session must already
// hold its history; once released, the local-only entries and the dropped
// q2..q4 are all on the first page, and the twins q1/q5 are not.
func TestPersistGapFill_InjectDoesNotWait(t *testing.T) {
	t.Parallel()
	const key = "feishu:p2p:gapfill-wait"
	fb := &blockingFallback{release: make(chan struct{})}
	for i := 1; i <= 5; i++ {
		fb.entries = append(fb.entries, clievent.EventEntry{UUID: fmt.Sprintf("c%d", i), Time: int64(i*1000 + 5), Type: "user", Detail: fmt.Sprintf("q%d", i)})
	}
	r, s := gapLoaderRouter(t, key, []string{
		`{"uuid":"l1","time":1000,"type":"user","detail":"q1"}`,
		`{"uuid":"l1t","time":1200,"type":"tool_use","detail":"Bash ls"}`,
		`{"time":5000,"type":"persist_gap","detail":"dropped=3"}`,
		`{"uuid":"l5","time":5000,"type":"user","detail":"q5"}`,
		`{"uuid":"l5k","time":5000,"type":"thinking","detail":"hmm"}`,
	}, []int64{1000, 1200, 5000, 5000, 5000}, fb)
	released := false
	release := func() {
		if !released {
			released = true
			close(fb.release)
		}
	}
	t.Cleanup(release)
	r.startBackgroundHistoryLoaders()
	testhelper.Eventually(t, s.hasInjectedHistory, 5*time.Second,
		"tier 1 did not inject while the fallback read was blocked")
	release()
	r.hist.wg.Wait()

	page, _ := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, 0)
	got := map[string]int{}
	for _, e := range page {
		got[e.UUID]++
	}
	for _, u := range []string{"l1", "l1t", "l5", "l5k", "c2", "c3", "c4"} {
		if got[u] != 1 {
			t.Errorf("%s served %d times on the first page, want 1; page %v", u, got[u], got)
		}
	}
	if got["c1"] > 0 || got["c5"] > 0 {
		t.Errorf("a twin of a local turn leaked: %v", got)
	}
	if mem := s.SnapshotPersistedHistory(); len(mem) != 5 {
		t.Errorf("persistedHistory holds %d entries, want the 5 local ones: fill turns stay off it", len(mem))
	}
}

// TestPersistGapFill_GapRecordBelowCut: the gap record sits below the newest
// maxPersistedHistory entries tier 1 injects. The 2x look-back still sees it,
// so the dropped question is served on a load-earlier page; the injection
// itself is exactly the newest maxPersistedHistory entries.
func TestPersistGapFill_GapRecordBelowCut(t *testing.T) {
	t.Parallel()
	const key = "feishu:p2p:gapfill-cut"
	records := []string{
		`{"uuid":"l0","time":1000,"type":"user","detail":"q0"}`,
		`{"time":20000,"type":"persist_gap","detail":"dropped=1"}`,
	}
	times := []int64{1000, 20000}
	for i := range maxPersistedHistory {
		ts := int64(20000 + i*10)
		records = append(records, fmt.Sprintf(`{"uuid":"t%d","time":%d,"type":"tool_use","detail":"x"}`, i, ts))
		times = append(times, ts)
	}
	fb := &blockingFallback{release: make(chan struct{}), entries: []clievent.EventEntry{
		{UUID: "c0", Time: 1005, Type: "user", Detail: "q0"},
		{UUID: "cq", Time: 20500, Type: "user", Detail: "dropped question"},
	}}
	close(fb.release)
	r, s := gapLoaderRouter(t, key, records, times, fb)
	r.startBackgroundHistoryLoaders()
	r.hist.wg.Wait()

	mem := s.SnapshotPersistedHistory()
	if len(mem) != maxPersistedHistory || mem[0].UUID != "t0" || mem[len(mem)-1].UUID != fmt.Sprintf("t%d", maxPersistedHistory-1) {
		first, last := "", ""
		if len(mem) > 0 {
			first, last = mem[0].UUID, mem[len(mem)-1].UUID
		}
		t.Fatalf("injected %d entries %s..%s, want the newest %d (t0..t%d)", len(mem), first, last, maxPersistedHistory, maxPersistedHistory-1)
	}
	served := map[string]int{}
	before := int64(0)
	for range 20 {
		pg := s.EventEntriesBeforeCtx(context.Background(), before, 100)
		if len(pg) == 0 {
			break
		}
		for _, e := range pg {
			served[e.UUID]++
		}
		before = pg[0].Time
	}
	if served["cq"] != 1 {
		t.Fatalf("dropped question behind a gap record below the cut served %d times, want 1", served["cq"])
	}
	if served["c0"] != 0 {
		t.Errorf("twin c0 of local l0 leaked")
	}
}

// TestWithGapFill_BoundaryOnce: a fill turn whose Time equals a page's first
// entry belongs to that page only; the next page's range ends strictly below
// it. Paging one entry at a time serves it, and each memory entry, once.
func TestWithGapFill_BoundaryOnce(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	var mem []clievent.EventEntry
	for i := range 6 {
		mem = append(mem, clievent.EventEntry{UUID: fmt.Sprintf("m%d", i), Time: int64(1000 * (i + 1)), Type: "tool_use"})
	}
	s.InjectHistoryIfEmpty(mem)
	gf := []clievent.EventEntry{
		{UUID: "g3", Time: 3000, Type: "user", Detail: "on the boundary"},
		{UUID: "g35", Time: 3500, Type: "user", Detail: "inside"},
	}
	s.gapFillCell().turns.Store(&gf)

	var order []string
	seen := map[string]int{}
	before := int64(4000)
	for range 10 {
		pg := s.EventEntriesBeforeCtx(context.Background(), before, 1)
		if len(pg) == 0 {
			break
		}
		for _, e := range pg {
			seen[e.UUID]++
			order = append(order, e.UUID)
		}
		if pg[0].UUID[0] != 'm' {
			t.Fatalf("page starts with fill turn %s: the next cursor would move", pg[0].UUID)
		}
		before = pg[0].Time
	}
	for _, u := range []string{"m0", "m1", "m2", "g3", "g35"} {
		if seen[u] != 1 {
			t.Errorf("%s served %d times, want 1 (order %v)", u, seen[u], order)
		}
	}
	if want := []string{"m2", "g3", "g35", "m1", "m0"}; !slices.Equal(order, want) {
		t.Errorf("serve order %v, want %v (fill turns after the page entry of equal Time)", order, want)
	}
}

// TestWithGapFill_InitialPageOnlyAdds: the initial page decides its disk
// top-up on the memory entries alone, so it is the page served without a gap
// fill plus the fill turns. Here memory holds 3 of the 5 visible bubbles
// wanted and the fill holds 5 more; the disk pages must still be read.
func TestWithGapFill_InitialPageOnlyAdds(t *testing.T) {
	t.Parallel()
	var disk []clievent.EventEntry
	for i := 1; i <= 20; i++ {
		disk = append(disk, clievent.EventEntry{UUID: fmt.Sprintf("d%d", i), Time: int64(i), Type: "text", Detail: "disk"})
	}
	build := func() *ManagedSession {
		s := &ManagedSession{key: "k"}
		s.SetHistorySource(&pagingHistorySource{all: disk})
		for i := 1; i <= 3; i++ {
			s.persistedHistory = append(s.persistedHistory, clievent.EventEntry{UUID: fmt.Sprintf("m%d", i), Time: int64(1000 + 10*i), Type: "text", Detail: "mem"})
		}
		return s
	}
	without, with := build(), build()
	var gf []clievent.EventEntry
	for i := 1; i <= 5; i++ {
		gf = append(gf, clievent.EventEntry{UUID: fmt.Sprintf("g%d", i), Time: int64(1010 + i), Type: "user", Detail: "fill"})
	}
	with.gapFillCell().turns.Store(&gf)

	ctx := context.Background()
	want := uuidsOf(without.EventLastNVisibleCtx(ctx, 5, 100))
	got := with.EventLastNVisibleCtx(ctx, 5, 100)
	var rest []string
	fills := 0
	for _, e := range got {
		if e.UUID[0] == 'g' {
			fills++
			continue
		}
		rest = append(rest, e.UUID)
	}
	if !slices.Equal(rest, want) || fills != len(gf) {
		t.Fatalf("initial page = %v, want %v plus the %d fill turns", uuidsOf(got), want, len(gf))
	}
}

// TestWithGapFill_DiskErrorKeepsFill: a short memory page whose disk top-up
// fails is served as end-of-history, and it still carries its fill turns.
func TestWithGapFill_DiskErrorKeepsFill(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistoryIfEmpty([]clievent.EventEntry{
		{UUID: "m1", Time: 1000, Type: "user", Detail: "q1"},
		{UUID: "m3", Time: 3000, Type: "user", Detail: "q3"},
	})
	gf := []clievent.EventEntry{{UUID: "g2", Time: 2000, Type: "user", Detail: "q2"}}
	s.gapFillCell().turns.Store(&gf)
	src := &fakeHistorySource{err: errors.New("disk read failed")}
	s.SetHistorySource(src)

	pg := s.EventEntriesBeforeCtx(context.Background(), 9000, 5)
	if src.calls != 1 {
		t.Fatalf("disk tier read %d times, want 1 (the memory page is short)", src.calls)
	}
	if got, want := uuidsOf(pg), []string{"m1", "g2", "m3"}; !slices.Equal(got, want) {
		t.Fatalf("page = %v, want %v (the fill turn survives the failed top-up)", got, want)
	}
}

func uuidsOf(es []clievent.EventEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.UUID)
	}
	return out
}

// TestWithGapFill_SkipsUUIDOnPage: a later InjectHistory (the shim and drift
// paths of #3028) can append the transcript tail after tier 1 filled the
// gap, so memory holds the fill turns' own rows. Each UUID is still served
// once, on the initial page and on load-earlier pages.
func TestWithGapFill_SkipsUUIDOnPage(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistoryIfEmpty([]clievent.EventEntry{
		{UUID: "l1", Time: 1000, Type: "user", Detail: "q1"},
		{UUID: "l5", Time: 5000, Type: "user", Detail: "q5"},
	})
	var gf []clievent.EventEntry
	for i := 2; i <= 4; i++ {
		gf = append(gf, clievent.EventEntry{UUID: fmt.Sprintf("c%d", i), Time: int64(i*1000 + 5), Type: "user", Detail: fmt.Sprintf("q%d", i)})
	}
	s.gapFillCell().turns.Store(&gf)
	s.InjectHistory(slices.Clone(gf))

	ctx := context.Background()
	page, _ := s.EventInitialPageCtx(ctx, DefaultVisibleTarget, 0)
	got := map[string]int{}
	for _, e := range page {
		got[e.UUID]++
	}
	for _, u := range []string{"l1", "l5", "c2", "c3", "c4"} {
		if got[u] != 1 {
			t.Errorf("initial page serves %s %d times, want 1: %v", u, got[u], uuidsOf(page))
		}
	}
	seen := map[string]int{}
	before := int64(9000)
	for range 20 {
		pg := s.EventEntriesBeforeCtx(ctx, before, 2)
		if len(pg) == 0 {
			break
		}
		for _, e := range pg {
			seen[e.UUID]++
		}
		before = pg[0].Time
	}
	for _, u := range []string{"l1", "l5", "c2", "c3", "c4"} {
		if seen[u] != 1 {
			t.Errorf("load-earlier serves %s %d times, want 1: %v", u, seen[u], seen)
		}
	}
}

// TestGapFill_FollowsTheLogicalSession: a respawn and a rename replace the
// ManagedSession struct; the fill rows stay reachable on the new one, on the
// initial page and on load-earlier pages. A fill tier 1 stores into a struct
// a spawn has already replaced (the slow fallback read) reaches the live one.
func TestGapFill_FollowsTheLogicalSession(t *testing.T) {
	t.Parallel()
	mem := []clievent.EventEntry{
		{UUID: "l1", Time: 1000, Type: "user", Detail: "q1"},
		{UUID: "l5", Time: 5000, Type: "user", Detail: "q5"},
	}
	gf := []clievent.EventEntry{{UUID: "c3", Time: 3005, Type: "user", Detail: "q3"}}
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) {
		// fakeProcess drops InjectHistory, so it starts with the history the
		// spawn path would have injected.
		return &fakeProcess{isAlive: true, entries: slices.Clone(mem)}, nil
	})
	ctx := context.Background()
	servesFill := func(t *testing.T, s *ManagedSession) {
		t.Helper()
		page, _ := s.EventInitialPageCtx(ctx, DefaultVisibleTarget, 0)
		if got := uuidsOf(page); !slices.Equal(got, []string{"l1", "c3", "l5"}) {
			t.Errorf("initial page %v, want [l1 c3 l5]", got)
		}
		seen := map[string]int{}
		before := int64(0)
		for range 10 {
			pg := s.EventEntriesBeforeCtx(ctx, before, 1)
			if len(pg) == 0 {
				break
			}
			for _, e := range pg {
				seen[e.UUID]++
			}
			before = pg[0].Time
		}
		if seen["c3"] != 1 {
			t.Errorf("load-earlier serves the fill row %d times, want 1: %v", seen["c3"], seen)
		}
	}

	t.Run("respawn and rename", func(t *testing.T) {
		const key, renamed = "feishu:p2p:gapfill-follow", "feishu:p2p:gapfill-follow-renamed"
		old := injectSession(r, key, nil)
		old.InjectHistoryIfEmpty(slices.Clone(mem))
		old.gapFillCell().turns.Store(&gf)
		s, err := spawnIn(r, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s == old {
			t.Fatal("test premise broken: the respawn must install a new struct")
		}
		servesFill(t, s)
		if !r.RenameSession(key, renamed) {
			t.Fatal("RenameSession returned false")
		}
		servesFill(t, r.SessionFor(renamed))
	})

	t.Run("fill stored after the respawn", func(t *testing.T) {
		const key = "feishu:p2p:gapfill-late"
		old := injectSession(r, key, nil)
		local := []clievent.EventEntry{
			mem[0],
			{Time: 5000, Type: schema.GapEntryType, Detail: "dropped=1"},
			mem[1],
		}
		old.SetHistorySource(&merged.Source{Local: gapSrc{local}, Fallback: gapSrc{gf}})
		old.InjectHistoryIfEmpty(slices.Clone(mem))
		s, err := spawnIn(r, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		old.fillPersistGaps(ctx, local, mem[0].Time)
		servesFill(t, s)
	})
}

// TestGapFillCell_ConcurrentFirstUseAgrees: tier 1 storing a fill and a
// successor's publish may both find s without a cell; every caller must get
// the cell s keeps, or the fill lands in a cell the live struct never reads.
func TestGapFillCell_ConcurrentFirstUseAgrees(t *testing.T) {
	t.Parallel()
	for range 500 {
		s := &ManagedSession{key: "k"}
		start := make(chan struct{})
		got := make([]*gapFillCell, 4)
		var wg sync.WaitGroup
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i] = s.gapFillCell()
			}()
		}
		close(start)
		wg.Wait()
		for i, c := range got {
			if c != s.gapFill.Load() {
				t.Fatalf("caller %d got a cell s does not keep", i)
			}
		}
	}
}
