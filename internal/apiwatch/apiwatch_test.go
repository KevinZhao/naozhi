package apiwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		r    Record
		want Class
	}{
		{"ok turn", Record{Kind: KindTurn, APIMS: 900}, ClassNone},
		{"region", Record{Kind: KindTurn, Failed: true, Status: 400,
			Error: "API Error: 400 Access to Anthropic models is not allowed from unsupported countries, regions, or territories"}, ClassRegion},
		{"expired token", Record{Kind: KindTurn, Failed: true, Status: 403, Error: "The security token included in the request is expired"}, ClassAuth},
		{"cloud credential", Record{Kind: KindRetry, Category: "cloud_credential_error"}, ClassAuth},
		{"rate limit", Record{Kind: KindRetry, Status: 429, Category: "rate_limit"}, ClassRateLimit},
		{"overloaded", Record{Kind: KindRetry, Status: 529, Category: "overloaded"}, ClassOverloaded},
		{"5xx", Record{Kind: KindRetry, Status: 503, Category: "server_error"}, ClassServer},
		{"4xx", Record{Kind: KindRetry, Status: 400}, ClassClient},
		{"first byte", Record{Kind: KindRetry, WaitedMS: 30000}, ClassFirstByte},
		{"connection", Record{Kind: KindRetry, Category: "unknown"}, ClassNetwork},
		{"timed out turn", Record{Kind: KindTurn, Failed: true, Error: "API Error: Request timed out."}, ClassTimeout},
		{"watchdog", Record{Kind: KindTimeout, Reason: "no_output_timeout"}, ClassWatchdog},
		{"other failure", Record{Kind: KindTurn, Failed: true, Error: "max turns reached"}, ClassOther},
	}
	for _, c := range cases {
		if got := c.r.Classify(); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// fixedClock pins the store's clock; advance moves it.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func openAt(t *testing.T, dir string, clk *fixedClock) *Store {
	t.Helper()
	s := &Store{dir: dir, now: clk.now, seen: map[string]struct{}{}}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		s.prune()
		s.load()
	}
	t.Cleanup(s.Close)
	return s
}

func TestStorePersistsReloadsAndDedups(t *testing.T) {
	dir := t.TempDir()
	clk := &fixedClock{t: time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)}
	s := openAt(t, dir, clk)
	s.Record(Record{Kind: KindRetry, Key: "k", Status: 429, UUID: "u1"})
	s.Record(Record{Kind: KindRetry, Key: "k", Status: 429, UUID: "u1"}) // same frame again
	s.Record(Record{Kind: KindTurn, Key: "k", APIMS: 1200, TTFTMS: 800, UUID: "u2"})
	s.Close()

	data, err := os.ReadFile(filepath.Join(dir, "2026-11-23.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("day file has %d lines, want 2:\n%s", n, data)
	}

	s2 := openAt(t, dir, clk)
	s2.Record(Record{Kind: KindRetry, Key: "k", UUID: "u1"}) // seen before the restart
	rep := s2.Report(Query{Window: time.Hour})
	if rep.Summary.Retries != 1 || rep.Summary.Turns != 1 || rep.Summary.RetriedTurns != 1 {
		t.Fatalf("summary after reload = %+v", rep.Summary)
	}
}

func TestStorePrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "2026-10-01.jsonl")
	keep := filepath.Join(dir, "2026-11-20.jsonl")
	for _, p := range []string{old, keep} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	openAt(t, dir, &fixedClock{t: time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("file past %d days kept", keepDays)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("recent file removed: %v", err)
	}
}

func TestReport(t *testing.T) {
	clk := &fixedClock{t: time.Date(2026, 11, 23, 10, 30, 0, 0, time.Local)}
	s := openAt(t, "", clk)
	at := func(m int) time.Time { return clk.t.Add(-time.Duration(m) * time.Minute) }
	// Outside the window: ignored.
	s.Record(Record{At: clk.t.Add(-3 * time.Hour), Kind: KindTurn, Key: "a", Failed: true})
	s.Record(Record{At: at(50), Kind: KindRetry, Key: "a", Category: "unknown"})
	s.Record(Record{At: at(49), Kind: KindTurn, Key: "a", APIMS: 5000, TTFTMS: 3000})
	s.Record(Record{At: at(20), Kind: KindTurn, Key: "b", APIMS: 1000, TTFTMS: 500})
	s.Record(Record{At: at(10), Kind: KindTurn, Key: "b", Failed: true, Status: 403, Error: "security token expired"})
	s.Record(Record{At: at(5), Kind: KindTimeout, Key: "c", Reason: "no_output_timeout"})

	rep := s.Report(Query{Window: time.Hour})
	sum := rep.Summary
	if sum.Turns != 3 || sum.FailedTurns != 1 || sum.Retries != 1 || sum.Timeouts != 1 || sum.RetriedTurns != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.API.N != 2 || sum.API.Max != 5000 || sum.TTFT.P50 != 3000 {
		t.Errorf("latency = %+v / %+v", sum.API, sum.TTFT)
	}
	if rep.BucketSec != 300 || len(rep.Buckets) != 12 {
		t.Errorf("buckets: size %ds, %d of them", rep.BucketSec, len(rep.Buckets))
	}
	var total int
	for _, b := range rep.Buckets {
		total += b.Turns
	}
	if total != 3 {
		t.Errorf("bucketed turns = %d, want 3", total)
	}
	if len(rep.Recent) != 3 || rep.Recent[0].Class != ClassWatchdog {
		t.Fatalf("recent = %+v", rep.Recent)
	}
	if got := len(rep.Classes); got != 3 {
		t.Errorf("classes = %+v", rep.Classes)
	}
	if len(rep.Slowest) != 2 || rep.Slowest[0].APIMS != 5000 {
		t.Errorf("slowest = %+v", rep.Slowest)
	}
	if one := s.Report(Query{Window: time.Hour, Key: "b"}); one.Summary.Turns != 2 || one.Summary.Retries != 0 {
		t.Errorf("key filter summary = %+v", one.Summary)
	}
}

func TestNilStore(t *testing.T) {
	var s *Store
	s.Record(Record{Kind: KindTurn})
	s.Close()
	if rep := s.Report(Query{}); rep.Summary.Turns != 0 || rep.Recent == nil {
		t.Fatalf("nil store report = %+v", rep)
	}
}

func TestErrorClipped(t *testing.T) {
	s := openAt(t, "", &fixedClock{t: time.Now()})
	s.Record(Record{Kind: KindTurn, Failed: true, Error: strings.Repeat("错", 1000)})
	rep := s.Report(Query{Window: time.Hour})
	if n := len([]rune(rep.Recent[0].Error)); n > maxErrorRunes+1 {
		t.Fatalf("error kept %d runes", n)
	}
}
