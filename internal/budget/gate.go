package budget

import (
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// Action is what a limit does once spend reaches it.
type Action string

const (
	ActionBlock Action = "block"
	ActionWarn  Action = "warn"
)

// DefaultWarnRatio is the share of a limit at which Verdict.Warn turns on.
const DefaultWarnRatio = 0.8

// Limits are the daily USD caps; 0 turns one off. Spend metered in another
// unit (credits, tokens) counts toward none.
type Limits struct {
	PerChatDailyUSD float64 // each IM chat; each project planner as one
	PerJobDailyUSD  float64 // each cron job
	DailyUSD        float64 // everything booked, dashboard included
	WarnRatio       float64 // 0 means DefaultWarnRatio
	Action          Action  // "" means ActionBlock
}

// Enabled reports whether any cap is set.
func (l Limits) Enabled() bool {
	return l.PerChatDailyUSD > 0 || l.PerJobDailyUSD > 0 || l.DailyUSD > 0
}

// Verdict is one check's outcome for the subject nearest its limit. The zero
// Verdict (no limit applies) admits.
type Verdict struct {
	Subject Subject
	Spent   float64
	Limit   float64
	Warn    bool // Spent has reached WarnRatio of Limit
	Over    bool // Spent has reached Limit
	Blocked bool // Over, and the action is block
	// ResetAt is the next midnight in the budget's time zone, when Spent
	// goes back to 0; zero when no limit applies.
	ResetAt time.Time
}

// Usage renders Spent against Limit, e.g. "$4.20 / $5.00".
func (v Verdict) Usage() string {
	return "$" + strconv.FormatFloat(v.Spent, 'f', 2, 64) + " / $" + strconv.FormatFloat(v.Limit, 'f', 2, 64)
}

// Notice is what a once-a-day message about a subject reports.
type Notice uint8

const (
	NoticeWarn    Notice = iota // spend reached warn_ratio
	NoticeOver                  // spend reached the cap under action warn
	NoticeBlocked               // a run or turn was refused
)

// Gate checks today's spend against Limits. A nil Gate admits everything.
type Gate struct {
	lim atomic.Pointer[Limits] // swapped by SetLimits; each check reads it once
	idx *Index
}

// NewGate returns a Gate over idx, or nil when lim sets no cap.
func NewGate(lim Limits, idx *Index) *Gate {
	if !lim.Enabled() || idx == nil {
		return nil
	}
	g := &Gate{idx: idx}
	g.SetLimits(lim)
	return g
}

// SetLimits replaces the caps for every later check (a config reload). A
// limits value with no cap leaves the gate admitting everything; the index
// keeps counting, so caps set again later see today's spend. No-op on nil.
func (g *Gate) SetLimits(lim Limits) {
	if g == nil {
		return
	}
	if lim.WarnRatio <= 0 || lim.WarnRatio > 1 {
		lim.WarnRatio = DefaultWarnRatio
	}
	if lim.Action != ActionWarn {
		lim.Action = ActionBlock
	}
	g.lim.Store(&lim)
}

// Enabled reports whether the gate currently enforces any cap.
func (g *Gate) Enabled() bool {
	return g != nil && g.lim.Load().Enabled()
}

// Attach builds a Gate fed by store: it counts today's entries already in
// the ledger, then each new one as it is written. Call it once at startup.
// It returns nil (admit all) when lim sets no cap or the ledger is off.
func Attach(store *costledger.Store, lim Limits, loc *time.Location, now func() time.Time) *Gate {
	if !lim.Enabled() {
		return nil
	}
	if !store.Enabled() {
		slog.Warn("budget: limits configured but the cost ledger is off; nothing is enforced")
		return nil
	}
	idx := NewIndex(loc, now)
	store.Subscribe(StartOfDay(idx.now(), idx.loc), idx.Add)
	return NewGate(lim, idx)
}

// CheckKey checks the session key's chat, project or cron job, and Global.
func (g *Gate) CheckKey(sessionKey string) Verdict {
	return g.check(SubjectForKey(sessionKey))
}

// CheckJob checks cron job id, and Global.
func (g *Gate) CheckJob(id string) Verdict {
	return g.check(JobSubject(id))
}

// Once reports true the first time it is asked about n for s each day, so
// each kind of notice goes out at most once a day per subject.
func (g *Gate) Once(n Notice, s Subject) bool {
	return g != nil && g.idx.firstNotice(n, s)
}

func (g *Gate) check(scoped Subject) Verdict {
	var v Verdict
	if g == nil {
		return v
	}
	lim := g.lim.Load()
	best := 0.0
	for _, s := range []Subject{scoped, Global} {
		limit := lim.limitFor(s)
		if limit <= 0 {
			continue
		}
		spent := g.idx.Spent(s)
		if r := spent / limit; v.Limit == 0 || r > best {
			best = r
			v = Verdict{Subject: s, Spent: spent, Limit: limit,
				Warn: r >= lim.WarnRatio, Over: r >= 1}
		}
	}
	if v.Limit > 0 {
		v.Blocked = v.Over && lim.Action == ActionBlock
		v.ResetAt = StartOfDay(g.idx.now(), g.idx.loc).AddDate(0, 0, 1)
	}
	return v
}

func (l Limits) limitFor(s Subject) float64 {
	switch {
	case s == "":
		return 0
	case s == Global:
		return l.DailyUSD
	case strings.HasPrefix(string(s), jobPrefix):
		return l.PerJobDailyUSD
	}
	return l.PerChatDailyUSD
}
