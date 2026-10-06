package budget

import (
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

const dayLayout = "2006-01-02"

// Index sums today's USD spend per Subject and in Global, where today is the
// calendar day in loc at now(). Entries dated any other day are ignored and
// the sums reset when the day turns. Safe for concurrent use.
type Index struct {
	loc *time.Location
	now func() time.Time

	mu     sync.Mutex
	day    string
	spent  map[Subject]float64
	warned map[Subject]bool
}

// NewIndex returns an empty index; nil loc means time.Local, nil now
// time.Now.
func NewIndex(loc *time.Location, now func() time.Time) *Index {
	if loc == nil {
		loc = time.Local
	}
	if now == nil {
		now = time.Now
	}
	return &Index{loc: loc, now: now, spent: map[Subject]float64{}, warned: map[Subject]bool{}}
}

// Add counts e if it is a USD entry dated today. Adjust rows count too: a
// negative one is a reconcile correction.
func (x *Index) Add(e costledger.Entry) {
	if e.Unit != costledger.UnitUSD || e.Amount == 0 {
		return
	}
	day := e.TS.In(x.loc).Format(dayLayout)
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.rollLocked() != day {
		return
	}
	x.spent[Global] += e.Amount
	if s := subjectFor(e); s != "" {
		x.spent[s] += e.Amount
	}
}

// Spent is s's USD total today.
func (x *Index) Spent(s Subject) float64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.rollLocked()
	return x.spent[s]
}

// firstWarn reports whether s has not been marked today, and marks it.
func (x *Index) firstWarn(s Subject) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.rollLocked()
	if x.warned[s] {
		return false
	}
	x.warned[s] = true
	return true
}

// rollLocked clears the sums when now() has moved to another day, and
// returns that day.
func (x *Index) rollLocked() string {
	today := x.now().In(x.loc).Format(dayLayout)
	if today != x.day {
		x.day = today
		clear(x.spent)
		clear(x.warned)
	}
	return today
}

// StartOfDay is midnight in loc of the day t falls on.
func StartOfDay(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
