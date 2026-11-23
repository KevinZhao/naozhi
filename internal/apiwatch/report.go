package apiwatch

import (
	"cmp"
	"slices"
	"time"
)

// Query selects a Report's window.
type Query struct {
	// Window is how far back from now; clamped to (0, memDays].
	Window time.Duration
	// Key, when set, keeps one session's records only.
	Key string
	// Recent caps the problem records listed; 0 means 100.
	Recent int
}

// Latency is a distribution of milliseconds; zero when nothing was measured.
type Latency struct {
	N   int   `json:"n"`
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	Max int64 `json:"max"`
}

// Summary totals a window.
type Summary struct {
	Turns       int `json:"turns"`
	FailedTurns int `json:"failed_turns"`
	Retries     int `json:"retries"`
	Timeouts    int `json:"timeouts"`
	// RetriedTurns counts the turns that had at least one retry.
	RetriedTurns int `json:"retried_turns"`
	// TTFT is the time from the turn's start to its first token (the CLI's
	// ttft_ms, request preparation included), API the time a turn spent in
	// model API calls.
	TTFT Latency `json:"ttft"`
	API  Latency `json:"api"`
}

// Bucket is one slice of the window.
type Bucket struct {
	Start    time.Time `json:"start"`
	Turns    int       `json:"turns"`
	Failed   int       `json:"failed"`
	Retries  int       `json:"retries"`
	Timeouts int       `json:"timeouts"`
	TTFTP95  int64     `json:"ttft_p95"`
	APIP95   int64     `json:"api_p95"`
}

// ClassCount is the problems of one class in the window.
type ClassCount struct {
	Class Class     `json:"class"`
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
	// Sample is the latest record of the class with an error text.
	Sample string `json:"sample,omitempty"`
}

// SessionCount is one session's problems and turns in the window.
type SessionCount struct {
	Key      string    `json:"key"`
	Turns    int       `json:"turns"`
	Problems int       `json:"problems"`
	Last     time.Time `json:"last"`
}

// Entry is a Record with its class.
type Entry struct {
	Record
	Class Class `json:"class,omitempty"`
}

// Report is what GET /api/system/apiwatch serves.
type Report struct {
	Since     time.Time      `json:"since"`
	Until     time.Time      `json:"until"`
	BucketSec int64          `json:"bucket_sec"`
	Summary   Summary        `json:"summary"`
	Buckets   []Bucket       `json:"buckets"`
	Classes   []ClassCount   `json:"classes"`
	Sessions  []SessionCount `json:"sessions"`
	Recent    []Entry        `json:"recent"`
	Slowest   []Entry        `json:"slowest"`
}

// bucketFor picks a bucket size giving at most ~48 buckets.
func bucketFor(w time.Duration) time.Duration {
	for _, b := range []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
		time.Hour, 3 * time.Hour, 6 * time.Hour} {
		if w/b <= 48 {
			return b
		}
	}
	return 12 * time.Hour
}

// Report summarises the window q selects.
func (s *Store) Report(q Query) Report {
	now := time.Now()
	if s != nil {
		now = s.now()
	}
	w := q.Window
	if w <= 0 || w > memDays*24*time.Hour {
		w = 24 * time.Hour
	}
	bsize := bucketFor(w)
	// Buckets align to bsize in local time so an hour bucket starts on the hour.
	_, off := now.Zone()
	shift := time.Duration(off) * time.Second
	start := now.Add(-w).Add(shift).Truncate(bsize).Add(-shift)
	rep := Report{Since: start, Until: now, BucketSec: int64(bsize / time.Second),
		Buckets: []Bucket{}, Classes: []ClassCount{}, Sessions: []SessionCount{}, Recent: []Entry{}, Slowest: []Entry{}}
	for t := start; t.Before(now); t = t.Add(bsize) {
		rep.Buckets = append(rep.Buckets, Bucket{Start: t})
	}
	if s == nil {
		return rep
	}
	limit := cmp.Or(q.Recent, 100)

	var ttft, api []int64
	bTTFT := make([][]int64, len(rep.Buckets))
	bAPI := make([][]int64, len(rep.Buckets))
	classes := map[Class]*ClassCount{}
	sessions := map[string]*SessionCount{}
	// retried remembers the sessions with a retry since their last turn.
	retried := map[string]bool{}
	var turns []Entry
	for _, r := range s.since(start) {
		if q.Key != "" && r.Key != q.Key {
			continue
		}
		bi := int(r.At.Sub(start) / bsize)
		if bi >= len(rep.Buckets) {
			bi = len(rep.Buckets) - 1
		}
		b := &rep.Buckets[bi]
		sc := sessions[r.Key]
		if sc == nil {
			sc = &SessionCount{Key: r.Key}
			sessions[r.Key] = sc
		}
		switch r.Kind {
		case KindRetry:
			rep.Summary.Retries++
			b.Retries++
			retried[r.Key] = true
		case KindTimeout:
			rep.Summary.Timeouts++
			b.Timeouts++
		case KindTurn:
			rep.Summary.Turns++
			b.Turns++
			sc.Turns++
			if retried[r.Key] {
				rep.Summary.RetriedTurns++
				delete(retried, r.Key)
			}
			if r.Failed {
				rep.Summary.FailedTurns++
				b.Failed++
			}
			if r.TTFTMS > 0 {
				ttft = append(ttft, r.TTFTMS)
				bTTFT[bi] = append(bTTFT[bi], r.TTFTMS)
			}
			if r.APIMS > 0 {
				api = append(api, r.APIMS)
				bAPI[bi] = append(bAPI[bi], r.APIMS)
				turns = append(turns, Entry{Record: r})
			}
		}
		if c := r.Classify(); c != ClassNone {
			sc.Problems++
			sc.Last = r.At
			cc := classes[c]
			if cc == nil {
				cc = &ClassCount{Class: c}
				classes[c] = cc
			}
			cc.Count++
			cc.Last = r.At
			if r.Error != "" {
				cc.Sample = r.Error
			}
			rep.Recent = append(rep.Recent, Entry{Record: r, Class: c})
		}
	}
	rep.Summary.TTFT, rep.Summary.API = latency(ttft), latency(api)
	for i := range rep.Buckets {
		rep.Buckets[i].TTFTP95 = latency(bTTFT[i]).P95
		rep.Buckets[i].APIP95 = latency(bAPI[i]).P95
	}
	for _, c := range Classes() {
		if cc := classes[c]; cc != nil {
			rep.Classes = append(rep.Classes, *cc)
		}
	}
	for _, sc := range sessions {
		if sc.Problems > 0 {
			rep.Sessions = append(rep.Sessions, *sc)
		}
	}
	slices.SortFunc(rep.Sessions, func(a, b SessionCount) int {
		return cmp.Or(cmp.Compare(b.Problems, a.Problems), b.Last.Compare(a.Last))
	})
	if len(rep.Sessions) > 10 {
		rep.Sessions = rep.Sessions[:10]
	}
	// Newest first.
	slices.Reverse(rep.Recent)
	if len(rep.Recent) > limit {
		rep.Recent = rep.Recent[:limit]
	}
	slices.SortFunc(turns, func(a, b Entry) int { return cmp.Compare(b.APIMS, a.APIMS) })
	if len(turns) > 10 {
		turns = turns[:10]
	}
	rep.Slowest = append(rep.Slowest, turns...)
	return rep
}

// latency is the distribution of v (sorted in place).
func latency(v []int64) Latency {
	if len(v) == 0 {
		return Latency{}
	}
	slices.Sort(v)
	at := func(p float64) int64 { return v[int(p*float64(len(v)-1)+0.5)] }
	return Latency{N: len(v), P50: at(0.5), P95: at(0.95), Max: v[len(v)-1]}
}
