package budget

import (
	"log/slog"
	"strings"
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

// Limits are the daily USD caps; 0 turns one off.
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
}

// Gate checks today's spend against Limits. A nil Gate admits everything.
type Gate struct {
	lim Limits
	idx *Index
}

// NewGate returns a Gate over idx, or nil when lim sets no cap.
func NewGate(lim Limits, idx *Index) *Gate {
	if !lim.Enabled() || idx == nil {
		return nil
	}
	if lim.WarnRatio <= 0 || lim.WarnRatio > 1 {
		lim.WarnRatio = DefaultWarnRatio
	}
	if lim.Action != ActionWarn {
		lim.Action = ActionBlock
	}
	return &Gate{lim: lim, idx: idx}
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

// CheckKey checks the session key's chat or project, and Global.
func (g *Gate) CheckKey(sessionKey string) Verdict {
	return g.check(SubjectForKey(sessionKey))
}

// CheckJob checks cron job id, and Global.
func (g *Gate) CheckJob(id string) Verdict {
	return g.check(JobSubject(id))
}

// ShouldWarnOnce reports true the first time it is asked about s each day.
func (g *Gate) ShouldWarnOnce(s Subject) bool {
	return g != nil && g.idx.firstWarn(s)
}

func (g *Gate) check(scoped Subject) Verdict {
	var v Verdict
	if g == nil {
		return v
	}
	best := -1.0
	for _, s := range []Subject{scoped, Global} {
		limit := g.limitFor(s)
		if limit <= 0 {
			continue
		}
		spent := g.idx.Spent(s)
		if r := spent / limit; r > best {
			best = r
			v = Verdict{Subject: s, Spent: spent, Limit: limit,
				Warn: r >= g.lim.WarnRatio, Over: r >= 1}
		}
	}
	v.Blocked = v.Over && g.lim.Action == ActionBlock
	return v
}

func (g *Gate) limitFor(s Subject) float64 {
	switch {
	case s == "":
		return 0
	case s == Global:
		return g.lim.DailyUSD
	case strings.HasPrefix(string(s), jobPrefix):
		return g.lim.PerJobDailyUSD
	}
	return g.lim.PerChatDailyUSD
}
