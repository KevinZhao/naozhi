package costledger

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// GroupBy names a summary dimension. Low-cardinality dimensions are served
// from the in-memory rollup when the window is covered; the others (and any
// filtered query) stream the day files in the window.
type GroupBy string

const (
	GroupBySource    GroupBy = "source"
	GroupByBackend   GroupBy = "backend"
	GroupByModel     GroupBy = "model"
	GroupByJob       GroupBy = "job"
	GroupBySession   GroupBy = "session"
	GroupByWorkspace GroupBy = "workspace"
	GroupByDay       GroupBy = "day"
	GroupByBasis     GroupBy = "basis"
	GroupByKind      GroupBy = "kind"
	GroupByUnit      GroupBy = "unit"
)

// MaxQueryDays is the default window cap; Query.AllowFullRange lifts it to
// the retention window (a 400-day scan reads every day file).
const MaxQueryDays = 90

// ErrBadQuery is returned for an unparsable or out-of-range Query.
var ErrBadQuery = errors.New("costledger: bad query")

// Query selects entries by window and optional filters and groups them.
type Query struct {
	From, To       time.Time
	GroupBy        GroupBy
	SessionKey     string
	JobID          string
	RunID          string
	Workspace      string
	AllowFullRange bool
}

// Bucket is one row of a Summary. Amount is summed per (Key, Unit); the same
// Key appears once per Unit so credits never mix with USD.
type Bucket struct {
	Key     string  `json:"key"`
	Unit    Unit    `json:"unit"`
	Amount  float64 `json:"amount"`
	Entries int     `json:"entries"`
	Tokens  Tokens  `json:"tokens"`
}

// Summary is the aggregate answer to a Query.
type Summary struct {
	From    time.Time      `json:"from"`
	To      time.Time      `json:"to"`
	Buckets []Bucket       `json:"buckets"`
	Basis   map[string]int `json:"basis"`
	Kinds   map[string]int `json:"kinds"`
	Dropped int64          `json:"dropped"`
}

func (g GroupBy) valid() bool {
	switch g {
	case GroupBySource, GroupByBackend, GroupByModel, GroupByJob, GroupBySession,
		GroupByWorkspace, GroupByDay, GroupByBasis, GroupByKind, GroupByUnit:
		return true
	}
	return false
}

func (q Query) filtered() bool {
	return q.SessionKey != "" || q.JobID != "" || q.RunID != "" || q.Workspace != ""
}

func (q Query) match(e Entry) bool {
	if e.TS.Before(q.From) || !e.TS.Before(q.To) {
		return false
	}
	if q.SessionKey != "" && e.SessionKey != q.SessionKey {
		return false
	}
	if q.JobID != "" && e.JobID != q.JobID {
		return false
	}
	if q.RunID != "" && e.RunID != q.RunID {
		return false
	}
	if q.Workspace != "" && e.Workspace != q.Workspace {
		return false
	}
	return true
}

// validate normalises the window and enforces the caps.
func (s *Store) validate(q *Query) error {
	if !q.GroupBy.valid() {
		return ErrBadQuery
	}
	if q.To.IsZero() {
		q.To = s.now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-30 * 24 * time.Hour)
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if !q.To.After(q.From) {
		return ErrBadQuery
	}
	limit := MaxQueryDays * 24 * time.Hour
	if q.AllowFullRange {
		// One day of slack: a caller spanning "retention ago" to "now (+ε)"
		// at sub-day precision exceeds the window by the fraction of today.
		limit = s.retention + 24*time.Hour
	}
	if q.To.Sub(q.From) > limit {
		return ErrBadQuery
	}
	return nil
}

// Summarize answers q. Disabled stores return an empty Summary.
func (s *Store) Summarize(q Query) (Summary, error) {
	if s == nil || s.disabled {
		return Summary{Buckets: []Bucket{}, Basis: map[string]int{}, Kinds: map[string]int{}}, nil
	}
	if err := s.validate(&q); err != nil {
		return Summary{}, err
	}
	agg := newAggregator(q.GroupBy)
	// The rollup holds every day from warmedFrom onward; a To in the future
	// matches no extra days, so only From needs the coverage check.
	if !q.filtered() && s.rollup.covers(q.From) && rollupServes(q.GroupBy) {
		s.rollup.fold(q, agg)
	} else {
		s.scanRange(q, func(e Entry) bool { agg.add(e); return true })
	}
	out := agg.summary()
	out.From, out.To, out.Dropped = q.From, q.To, s.dropped.Load()
	return out, nil
}

// Entries returns up to limit matching entries, newest first (audit/debug).
func (s *Store) Entries(q Query, limit int) ([]Entry, error) {
	if s == nil || s.disabled {
		return []Entry{}, nil
	}
	if q.GroupBy == "" {
		q.GroupBy = GroupByDay
	}
	if err := s.validate(&q); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	var out []Entry
	s.scanRange(q, func(e Entry) bool { out = append(out, e); return true })
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []Entry{}
	}
	return out, nil
}

// Scan streams every entry matching q (filters + window) through fn in day
// order, stopping when fn returns false. Backfill and audits use it; the
// same validation and caps as Summarize apply.
func (s *Store) Scan(q Query, fn func(Entry) bool) error {
	if s == nil || s.disabled {
		return nil
	}
	if q.GroupBy == "" {
		q.GroupBy = GroupByDay
	}
	if err := s.validate(&q); err != nil {
		return err
	}
	s.scanRange(q, fn)
	return nil
}

// scanRange streams every day file overlapping the window through fn,
// applying q's filters.
func (s *Store) scanRange(q Query, fn func(Entry) bool) {
	from, to := q.From.Format(dayLayout), q.To.Format(dayLayout)
	stopped := false
	for _, d := range s.dayFiles() {
		if stopped {
			return
		}
		if d < from || d > to {
			continue
		}
		s.scanDay(d, func(e Entry) bool {
			if q.match(e) && !fn(e) {
				stopped = true
				return false
			}
			return true
		})
	}
}

// rollupServes reports whether the dimension is pre-aggregated.
func rollupServes(g GroupBy) bool {
	switch g {
	case GroupBySource, GroupByBackend, GroupByDay, GroupByBasis, GroupByKind, GroupByUnit, GroupByModel:
		return true
	}
	return false
}

// rollup keeps per-day pre-aggregates over the low-cardinality dimensions.
// Model rows use ModelDelta.CostUSD (drill-down figures), every other
// dimension uses Entry.Amount (authoritative).
type rollup struct {
	mu         sync.RWMutex
	days       map[string]*dayAgg
	warmedFrom string // first day guaranteed loaded (dayLayout)
}

type lowKey struct {
	Unit    Unit
	Source  Source
	Backend string
	Basis   Basis
	Kind    Kind
}

type modelKey struct {
	Unit  Unit
	Model string
}

type dayAgg struct {
	low    map[lowKey]*Bucket
	models map[modelKey]*Bucket
}

func newRollup() *rollup { return &rollup{days: make(map[string]*dayAgg)} }

func (r *rollup) covers(from time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.warmedFrom != "" && from.Format(dayLayout) >= r.warmedFrom
}

func (r *rollup) add(e Entry) {
	day := e.TS.Format(dayLayout)
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.days[day]
	if d == nil {
		d = &dayAgg{low: make(map[lowKey]*Bucket), models: make(map[modelKey]*Bucket)}
		r.days[day] = d
	}
	lk := lowKey{e.Unit, e.Source, e.Backend, e.Basis, e.Kind}
	b := d.low[lk]
	if b == nil {
		b = &Bucket{Unit: e.Unit}
		d.low[lk] = b
	}
	b.Amount += e.Amount
	b.Entries++
	for _, m := range e.Models {
		b.Tokens = b.Tokens.add(m.Tokens)
		mk := modelKey{UnitUSD, m.Model}
		mb := d.models[mk]
		if mb == nil {
			mb = &Bucket{Unit: UnitUSD}
			d.models[mk] = mb
		}
		mb.Amount += m.CostUSD
		mb.Entries++
		mb.Tokens = mb.Tokens.add(m.Tokens)
	}
}

// fold replays the rollup for q's window into agg. Day granularity: a window
// starting mid-day includes that whole day (callers pass day-aligned windows
// for exact figures; the dashboard does).
func (r *rollup) fold(q Query, agg *aggregator) {
	from, to := q.From.Format(dayLayout), q.To.Format(dayLayout)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for day, d := range r.days {
		if day < from || day > to {
			continue
		}
		for lk, b := range d.low {
			agg.basis[string(lk.Basis)] += b.Entries
			agg.kinds[string(lk.Kind)] += b.Entries
			if q.GroupBy != GroupByModel {
				agg.merge(lowKeyLabel(q.GroupBy, day, lk), *b)
			}
		}
		if q.GroupBy == GroupByModel {
			for mk, b := range d.models {
				agg.merge(mk.Model, *b)
			}
		}
	}
}

func lowKeyLabel(g GroupBy, day string, k lowKey) string {
	switch g {
	case GroupBySource:
		return string(k.Source)
	case GroupByBackend:
		return k.Backend
	case GroupByDay:
		return day
	case GroupByBasis:
		return string(k.Basis)
	case GroupByKind:
		return string(k.Kind)
	default:
		return string(k.Unit)
	}
}

// aggregator merges entries or pre-aggregated buckets into (key, unit) rows.
type aggregator struct {
	groupBy GroupBy
	rows    map[modelKey]*Bucket
	basis   map[string]int
	kinds   map[string]int
}

func newAggregator(g GroupBy) *aggregator {
	return &aggregator{groupBy: g, rows: make(map[modelKey]*Bucket), basis: make(map[string]int), kinds: make(map[string]int)}
}

func (a *aggregator) merge(key string, b Bucket) {
	k := modelKey{b.Unit, key}
	row := a.rows[k]
	if row == nil {
		row = &Bucket{Key: key, Unit: b.Unit}
		a.rows[k] = row
	}
	row.Amount += b.Amount
	row.Entries += b.Entries
	row.Tokens = row.Tokens.add(b.Tokens)
}

func (a *aggregator) add(e Entry) {
	a.basis[string(e.Basis)]++
	a.kinds[string(e.Kind)]++
	if a.groupBy == GroupByModel {
		for _, m := range e.Models {
			a.merge(m.Model, Bucket{Unit: UnitUSD, Amount: m.CostUSD, Entries: 1, Tokens: m.Tokens})
		}
		return
	}
	var tok Tokens
	for _, m := range e.Models {
		tok = tok.add(m.Tokens)
	}
	a.merge(a.label(e), Bucket{Unit: e.Unit, Amount: e.Amount, Entries: 1, Tokens: tok})
}

func (a *aggregator) label(e Entry) string {
	switch a.groupBy {
	case GroupBySource:
		return string(e.Source)
	case GroupByBackend:
		return e.Backend
	case GroupByJob:
		return e.JobID
	case GroupBySession:
		return e.SessionKey
	case GroupByWorkspace:
		return e.Workspace
	case GroupByDay:
		return e.TS.Format(dayLayout)
	case GroupByBasis:
		return string(e.Basis)
	case GroupByKind:
		return string(e.Kind)
	default:
		return string(e.Unit)
	}
}

func (a *aggregator) summary() Summary {
	out := Summary{Buckets: make([]Bucket, 0, len(a.rows)), Basis: a.basis, Kinds: a.kinds}
	for _, b := range a.rows {
		out.Buckets = append(out.Buckets, *b)
	}
	sort.Slice(out.Buckets, func(i, j int) bool {
		if out.Buckets[i].Amount != out.Buckets[j].Amount {
			return out.Buckets[i].Amount > out.Buckets[j].Amount
		}
		if out.Buckets[i].Key != out.Buckets[j].Key {
			return out.Buckets[i].Key < out.Buckets[j].Key
		}
		return out.Buckets[i].Unit < out.Buckets[j].Unit
	})
	return out
}
