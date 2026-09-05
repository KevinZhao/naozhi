package costledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T, now time.Time) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cost")
	s := NewStore(dir, Options{Now: func() time.Time { return now }})
	if !s.Enabled() {
		t.Fatal("store should be enabled")
	}
	t.Cleanup(s.Close)
	return s, dir
}

func mk(ts time.Time, src Source, unit Unit, amt float64, mods ...ModelDelta) Entry {
	return Entry{TS: ts, Source: src, Kind: KindTurn, Backend: "claude", Unit: unit, Amount: amt, Basis: BasisList,
		SessionKey: "feishu:p2p:u1", JobID: "", Models: mods}
}

func TestStore_AppendPersistsJSONLWithPrivatePerms(t *testing.T) {
	s, dir := newTestStore(t, t0)
	e := mk(t0, SourceSession, UnitUSD, 0.25, ModelDelta{Model: "m", CostUSD: 0.25, Tokens: Tokens{Input: 10}})
	if !s.Append(e) {
		t.Fatal("append rejected")
	}
	s.Close()
	path := filepath.Join(dir, "2026-09-05.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != filePerm {
		t.Errorf("file perm = %o, want %o", fi.Mode().Perm(), filePerm)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != dirPerm {
		t.Errorf("dir perm = %o, want %o", di.Mode().Perm(), dirPerm)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	var got Entry
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Amount != 0.25 || got.Models[0].Input != 10 || got.Source != SourceSession {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestStore_BatchSpanningThreeDaysSplitsFiles(t *testing.T) {
	s, dir := newTestStore(t, t0)
	for _, ts := range []time.Time{t0.Add(-48 * time.Hour), t0.Add(-24 * time.Hour), t0} {
		if !s.Append(mk(ts, SourceSession, UnitUSD, 1)) {
			t.Fatal("append rejected")
		}
	}
	s.Close()
	for _, day := range []string{"2026-09-03", "2026-09-04", "2026-09-05"} {
		raw, err := os.ReadFile(filepath.Join(dir, day+fileSuffix))
		if err != nil || strings.Count(string(raw), "\n") != 1 {
			t.Fatalf("day %s: err=%v lines=%d", day, err, strings.Count(string(raw), "\n"))
		}
	}
}

func TestSummarize_FutureToUsesRollupAndMatchesScan(t *testing.T) {
	s, _ := newTestStore(t, t0)
	seed(t, s)
	q := Query{From: t0.Add(-72 * time.Hour), To: t0.Add(48 * time.Hour), GroupBy: GroupByUnit}
	sum, err := s.Summarize(q)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := bucket(sum, "USD", UnitUSD); b.Amount != 3.75 || b.Entries != 4 {
		t.Fatalf("USD total = %+v", b)
	}
}

func TestStore_RejectsInvalidWithoutCountingDrop(t *testing.T) {
	s, _ := newTestStore(t, t0)
	if s.Append(Entry{Source: "nope", Unit: UnitUSD, Kind: KindTurn, Amount: 1}) {
		t.Fatal("invalid source accepted")
	}
	if s.Append(mk(t0, SourceSession, UnitUSD, 0)) {
		t.Fatal("zero amount accepted")
	}
	if s.Dropped() != 0 {
		t.Fatalf("rejections must not count as drops, got %d", s.Dropped())
	}
}

func TestStore_QueueFullCountsDropped(t *testing.T) {
	s := &Store{dir: t.TempDir(), now: func() time.Time { return t0 }, ch: make(chan Entry, 1), rollup: newRollup()}
	if !s.Append(mk(t0, SourceSession, UnitUSD, 1)) {
		t.Fatal("first append should queue")
	}
	if s.Append(mk(t0, SourceSession, UnitUSD, 1)) {
		t.Fatal("second append should drop")
	}
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d", s.Dropped())
	}
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	day1, day2 := t0.Add(-48*time.Hour), t0.Add(-24*time.Hour)
	ents := []Entry{
		mk(day1, SourceSession, UnitUSD, 1.0, ModelDelta{Model: "fable", CostUSD: 1.0, Tokens: Tokens{Input: 100}}),
		mk(day1, SourceCronLocal, UnitUSD, 0.5, ModelDelta{Model: "opus", CostUSD: 0.5, Tokens: Tokens{Output: 20}}),
		mk(day2, SourceSession, UnitUSD, 2.0, ModelDelta{Model: "fable", CostUSD: 2.0}),
		mk(day2, SourceSession, UnitCredits, 3.0),
		mk(t0, SourceCronSandbox, UnitUSD, 0.25),
	}
	ents[1].JobID, ents[1].SessionKey = "job1", "cron:job1"
	ents[3].Backend, ents[3].Kind, ents[3].Basis = "kiro", KindMetering, BasisNone
	ents[4].Kind, ents[4].Basis, ents[4].JobID = KindReceipt, BasisNone, "job2"
	for i, e := range ents {
		if !s.Append(e) {
			t.Fatalf("seed %d rejected", i)
		}
	}
	s.Close()
}

func bucket(sum Summary, key string, unit Unit) (Bucket, bool) {
	for _, b := range sum.Buckets {
		if b.Key == key && b.Unit == unit {
			return b, true
		}
	}
	return Bucket{}, false
}

func TestSummarize_UnitsNeverMixAndRollupMatchesScan(t *testing.T) {
	s, dir := newTestStore(t, t0)
	seed(t, s)
	// Reopen so the rollup is warmed from disk (startup path), then compare
	// with a forced scan.
	s2 := NewStore(dir, Options{Now: func() time.Time { return t0.Add(time.Hour) }})
	defer s2.Close()
	q := Query{From: t0.Add(-72 * time.Hour), To: t0.Add(time.Hour), GroupBy: GroupBySource}
	viaRollup, err := s2.Summarize(q)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.rollup.covers(q.From) {
		t.Fatal("test premise: rollup should cover the window")
	}
	agg := newAggregator(GroupBySource)
	s2.scanRange(q, func(e Entry) bool { agg.add(e); return true })
	viaScan := agg.summary()
	if len(viaRollup.Buckets) != len(viaScan.Buckets) {
		t.Fatalf("rollup %+v vs scan %+v", viaRollup.Buckets, viaScan.Buckets)
	}
	for i := range viaScan.Buckets {
		if viaRollup.Buckets[i] != viaScan.Buckets[i] {
			t.Fatalf("bucket %d: rollup %+v vs scan %+v", i, viaRollup.Buckets[i], viaScan.Buckets[i])
		}
	}
	usd, _ := bucket(viaRollup, "session", UnitUSD)
	cr, _ := bucket(viaRollup, "session", UnitCredits)
	if usd.Amount != 3.0 || usd.Entries != 2 || usd.Tokens.Input != 100 {
		t.Fatalf("session USD = %+v", usd)
	}
	if cr.Amount != 3.0 || cr.Entries != 1 {
		t.Fatalf("session credits = %+v", cr)
	}
	if viaRollup.Basis["list"] != 3 || viaRollup.Basis[""] != 2 || viaRollup.Kinds["receipt"] != 1 {
		t.Fatalf("basis/kinds = %v / %v", viaRollup.Basis, viaRollup.Kinds)
	}
}

func TestSummarize_ModelDayJobAndFilters(t *testing.T) {
	s, _ := newTestStore(t, t0)
	seed(t, s)
	win := Query{From: t0.Add(-72 * time.Hour), To: t0.Add(time.Hour)}

	q := win
	q.GroupBy = GroupByModel
	sum, err := s.Summarize(q)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := bucket(sum, "fable", UnitUSD); b.Amount != 3.0 || b.Tokens.Input != 100 {
		t.Fatalf("fable = %+v", b)
	}
	if sum.Kinds["turn"] != 3 || sum.Kinds["metering"] != 1 {
		t.Fatalf("model query must still report kinds, got %v", sum.Kinds)
	}

	q = win
	q.GroupBy = GroupByDay
	sum, _ = s.Summarize(q)
	if b, _ := bucket(sum, t0.Add(-24*time.Hour).Format(dayLayout), UnitUSD); b.Amount != 2.0 {
		t.Fatalf("day bucket = %+v", b)
	}

	q = win
	q.GroupBy, q.JobID = GroupByJob, "job1"
	sum, _ = s.Summarize(q)
	if len(sum.Buckets) != 1 || sum.Buckets[0].Key != "job1" || sum.Buckets[0].Amount != 0.5 {
		t.Fatalf("job filter = %+v", sum.Buckets)
	}

	q = win
	q.GroupBy, q.SessionKey = GroupBySession, "feishu:p2p:u1"
	sum, _ = s.Summarize(q)
	if b, _ := bucket(sum, "feishu:p2p:u1", UnitUSD); b.Amount != 3.25 || b.Entries != 3 {
		t.Fatalf("session filter = %+v", sum.Buckets)
	}

	ents, err := s.Entries(Query{From: win.From, To: win.To, JobID: "job2"}, 10)
	if err != nil || len(ents) != 1 || ents[0].Kind != KindReceipt {
		t.Fatalf("entries = %+v err=%v", ents, err)
	}
}

func TestSummarize_ValidatesQuery(t *testing.T) {
	s, _ := newTestStore(t, t0)
	bad := []Query{
		{GroupBy: "nope", From: t0.Add(-time.Hour), To: t0},
		{GroupBy: GroupByDay, From: t0, To: t0.Add(-time.Hour)},
		{GroupBy: GroupByDay, From: t0.Add(-(MaxQueryDays + 1) * 24 * time.Hour), To: t0},
	}
	for i, q := range bad {
		if _, err := s.Summarize(q); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	ok := Query{GroupBy: GroupByDay, From: t0.Add(-(MaxQueryDays + 1) * 24 * time.Hour), To: t0, AllowFullRange: true}
	if _, err := s.Summarize(ok); err != nil {
		t.Errorf("full range should be allowed: %v", err)
	}
	def, err := s.Summarize(Query{GroupBy: GroupBySource})
	if err != nil || !def.To.Equal(t0) || def.To.Sub(def.From) != 30*24*time.Hour {
		t.Errorf("default window = %v..%v err=%v", def.From, def.To, err)
	}
}

func TestStore_SkipsHalfLineAndSweepsRetention(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cost")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(mk(t0.Add(-24*time.Hour), SourceSession, UnitUSD, 1))
	os.WriteFile(filepath.Join(dir, "2026-09-04.jsonl"), append(append(good, '\n'), []byte(`{"ts":"2026-09-04T`)...), filePerm)
	os.WriteFile(filepath.Join(dir, "2020-01-01.jsonl"), append(good, '\n'), filePerm)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), filePerm)
	s := NewStore(dir, Options{Now: func() time.Time { return t0 }, RetentionDays: 30})
	defer s.Close()
	if _, err := os.Stat(filepath.Join(dir, "2020-01-01.jsonl")); !os.IsNotExist(err) {
		t.Error("expired day file not swept")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("sweep must only touch day files")
	}
	sum, err := s.Summarize(Query{From: t0.Add(-48 * time.Hour), To: t0, GroupBy: GroupBySource})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := bucket(sum, "session", UnitUSD); b.Amount != 1 || b.Entries != 1 {
		t.Fatalf("half line handling: %+v", sum.Buckets)
	}
}

func TestStore_RefusesSymlinkDayFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cost")
	os.MkdirAll(dir, dirPerm)
	target := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(target, nil, 0o600)
	os.Symlink(target, filepath.Join(dir, "2026-09-05.jsonl"))
	s := NewStore(dir, Options{Now: func() time.Time { return t0 }})
	s.Append(mk(t0, SourceSession, UnitUSD, 1))
	s.Close()
	if raw, _ := os.ReadFile(target); len(raw) != 0 {
		t.Fatal("wrote through symlink")
	}
	if s.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", s.Dropped())
	}
}

func TestStore_ClampsDays(t *testing.T) {
	r, u := clampDays(0, 0)
	if r != DefaultRetentionDays || u != DefaultRollupDays {
		t.Fatalf("defaults = %d/%d", r, u)
	}
	r, u = clampDays(99999, 500)
	if r != MaxRetentionDays || u != 500 {
		t.Fatalf("clamped = %d/%d", r, u)
	}
	if r, u = clampDays(10, 20); u != 10 {
		t.Fatalf("rollup must not exceed retention: %d/%d", r, u)
	}
}

func TestStore_DisabledIsNoop(t *testing.T) {
	var s *Store
	if s.Append(mk(t0, SourceSession, UnitUSD, 1)) || s.Dropped() != 0 || s.Enabled() {
		t.Fatal("nil store must be a no-op")
	}
	d := NewStore("", Options{})
	if d.Enabled() || d.Append(mk(t0, SourceSession, UnitUSD, 1)) {
		t.Fatal("disabled store must be a no-op")
	}
	if sum, err := d.Summarize(Query{GroupBy: GroupBySource}); err != nil || len(sum.Buckets) != 0 {
		t.Fatalf("disabled summary = %+v err=%v", sum, err)
	}
	d.Close()
}
