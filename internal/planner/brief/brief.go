// Package brief is the morning brief, 早报 (docs/rfc/planner.md §10.1,
// §15.1): on each weekday, at the first reconcile after 08:30 local, the
// gateway lists what finished and what failed since the last brief (with
// what custody did about it), the cron jobs whose result changed, and what
// is still open in the inbox — how many "等你" items wait (the left column
// lists them), the unclosed errors, the notices (D5: the brief is where a
// notice is read, and it closes the ones it carried) and the 任务 not done
// (§15.4) — and, from the scout (§15.6 早报增强), today's calendar, the
// people waiting on a reply and the Asana tasks due, with what the Planner
// held for it (notify blocks of level brief) and a Midway credential that
// runs out before 09:00, as short Chinese Markdown, journals it
// (kind=brief) and pushes it once. It is rendered by fixed rules from the
// journal, the inbox, the health snapshot, the cron run history and the
// scout's state; nothing here calls a model.
//
// A day with nothing to report sends nothing. Its judgment is journaled all
// the same, so a restart later that day does not judge it again, and the
// next brief starts from it.
package brief

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Timing (§15.1).
const (
	// hour:minute local on a weekday is when the brief falls due.
	hour, minute = 8, 30
	// firstLookback is the first brief's window, with no earlier one.
	firstLookback = 24 * time.Hour
	// maxLookback caps the window after a long gap (weeks off, briefs
	// missing from the journal).
	maxLookback = 7 * 24 * time.Hour
	// keepRecent is how many sent briefs Recent can return; seedLookback the
	// journal span New reads them back from.
	keepRecent   = 14
	seedLookback = 21 * 24 * time.Hour
	// midwayBy is the local hour a Midway credential expiring before it is
	// in the brief: the morning's work would hit it.
	midwayBy = 9
)

// journalUnit is the unit of the brief's journal entries.
const journalUnit = "brief"

// Counts are a brief's items per section.
type Counts struct {
	Meetings  int `json:"meetings"`
	Replies   int `json:"replies"`
	Deadlines int `json:"deadlines"`
	Done      int `json:"done"`
	Failed    int `json:"failed"`
	Waiting   int `json:"waiting"`
	Errors    int `json:"errors"`
	Tasks     int `json:"tasks"`
	Planner   int `json:"planner"`
	// Routines are the cron jobs whose result changed, Notices the notices
	// carried, Custody what custody sent or stopped on, and Midway 1 when
	// the Midway credential runs out before 09:00.
	Routines int `json:"routines,omitempty"`
	Notices  int `json:"notices,omitempty"`
	Custody  int `json:"custody,omitempty"`
	Midway   int `json:"midway,omitempty"`
}

func (c Counts) total() int {
	return c.Meetings + c.Replies + c.Deadlines + c.Done + c.Failed + c.Waiting + c.Errors + c.Tasks + c.Planner +
		c.Routines + c.Notices + c.Custody + c.Midway
}

// Routine is a cron job's latest successful run in the window and whether
// its result changed from the run before (cron.Scheduler.LatestResultChange).
type Routine struct {
	Unit      string // cron:<job ID>
	At        time.Time
	Changed   bool
	FirstLine string
}

// Mention is something the Planner holds for the next brief (a notify block
// of level brief): Key names the thing, so a brief lists it once.
type Mention struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Why   string `json:"why,omitempty"`
	Link  string `json:"link,omitempty"`
}

// eventHold is the notify journal event of a held Mention.
const eventHold = "brief_hold"

type holdData struct {
	Event string `json:"event"`
	Mention
}

// Hold journals m for the next brief, whose window it falls in.
func (b *Briefer) Hold(m Mention) error {
	if b.elsewhere() {
		return ErrElsewhere
	}
	if b.cfg.Journal == nil {
		return errors.New("brief: no journal")
	}
	data, err := json.Marshal(holdData{Event: eventHold, Mention: m})
	if err != nil {
		return err
	}
	return b.cfg.Journal.Append(journal.Entry{TS: b.cfg.Now(), Kind: journal.KindNotify, Unit: sessionkey.PlannerMainKey, Data: data})
}

// Brief is one day's judgment, the data of a kind=brief journal entry.
type Brief struct {
	Day   string    `json:"day"` // local YYYY-MM-DD
	At    time.Time `json:"at"`
	Since time.Time `json:"since"`
	// Sent is false for a day with nothing to report: such a judgment only
	// keeps the day from being judged again and is never listed.
	Sent     bool   `json:"sent"`
	Counts   Counts `json:"counts"`
	Markdown string `json:"markdown,omitempty"`
}

// Config wires a Briefer. Every source may be nil, which leaves out what it
// feeds.
type Config struct {
	// Now is the wall clock; nil is time.Now.
	Now     func() time.Time
	Journal *journal.Journal
	Health  func() health.Snapshot
	// Inbox returns the inbox's items (only the open ones are read).
	Inbox func() []notify.Item
	// Tasks returns the 任务 list (task.Board.List); the promises past due
	// and the sessions waiting or failed are listed.
	Tasks func() []task.Task
	// Push sends the brief's notification (notify.Notifier.Announce).
	Push func(notify.Push) bool
	// Name returns a unit's display name ("" falls back to the unit ID).
	Name       func(unit string) string
	Workspaces *workspace.Registry
	// Calendar returns the scout's calendar cache (scout.Scout.Calendar);
	// today's meetings not over yet are listed.
	Calendar func() []scout.Event
	// Patrol returns the scout runs that went through and ended at or
	// after a time (scout.Scout.Since); the reply_needed findings and the
	// Asana tasks due of those since the last brief are listed.
	Patrol func(since time.Time) []scout.Run
	// Routines returns the cron jobs' latest successful runs in the window;
	// the changed ones are listed, the others counted.
	Routines func(since, until time.Time) []Routine
	// Midway returns when the local Midway credential expires, zero when
	// unknown; one expiring before 09:00 today is in the brief.
	Midway func() time.Time
	// Carried closes the notices a sent brief listed
	// (notify.Notifier.Carried).
	Carried func(ids []string)
	// Elsewhere reports that the brief is another node's to send: with
	// several nodes, only the one running the scout sends one, so a day is
	// not briefed twice (docs/rfc/remote-planner.md §2.1). nil is never.
	Elsewhere func() bool
}

// ErrElsewhere refuses a mention held for a brief this node does not send.
var ErrElsewhere = errors.New("早报只在跑巡检的那台机器上发，这台不发早报")

// Briefer judges the day's brief. Check runs on Run's goroutine; Recent is
// safe from any goroutine.
type Briefer struct {
	cfg  Config
	poke chan struct{}

	mu     sync.Mutex
	last   Brief   // the latest judgment, sent or not
	recent []Brief // sent briefs, oldest first, at most keepRecent
}

// New returns a Briefer that picks up after the briefs the journal holds.
func New(cfg Config) *Briefer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	b := &Briefer{cfg: cfg, poke: make(chan struct{}, 1)}
	if cfg.Journal == nil {
		return b
	}
	now := cfg.Now()
	entries, err := cfg.Journal.Read(now.Add(-seedLookback), now.Add(time.Minute))
	if err != nil {
		slog.Warn("brief: read journal for earlier briefs", "err", err)
	}
	for _, e := range entries {
		var br Brief
		if e.Kind != journal.KindBrief || json.Unmarshal(e.Data, &br) != nil || br.Day == "" {
			continue
		}
		b.keep(br)
	}
	return b
}

// keep records br as the latest judgment.
func (b *Briefer) keep(br Brief) {
	b.last = br
	if !br.Sent {
		return
	}
	b.recent = append(b.recent, br)
	if n := len(b.recent) - keepRecent; n > 0 {
		b.recent = append(b.recent[:0:0], b.recent[n:]...)
	}
}

// Recent returns up to limit sent briefs, newest first.
func (b *Briefer) Recent(limit int) []Brief {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []Brief{}
	for i := len(b.recent) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, b.recent[i])
	}
	return out
}

// Poke asks Run for a check: the health machine reconciled. Never blocks.
func (b *Briefer) Poke() {
	select {
	case b.poke <- struct{}{}:
	default:
	}
}

func (b *Briefer) elsewhere() bool { return b.cfg.Elsewhere != nil && b.cfg.Elsewhere() }

// Run checks after each Poke until ctx ends.
func (b *Briefer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.poke:
			b.safely()
		}
	}
}

func (b *Briefer) safely() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("brief: check panicked", "panic", r)
		}
	}()
	b.Check()
}

// due reports whether a brief falls due at now when the latest judgment was
// for lastDay: a weekday, 08:30 local or later, not judged yet. Waking from a
// sleep past 08:30 makes it due at the first check, once.
func due(now time.Time, lastDay string) bool {
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	y, m, d := now.Date()
	if now.Before(time.Date(y, m, d, hour, minute, 0, 0, now.Location())) {
		return false
	}
	return now.Format(time.DateOnly) != lastDay
}

// windowStart is where the brief judged at now starts: the latest judgment,
// within maxLookback.
func windowStart(now, last time.Time) time.Time {
	if last.IsZero() {
		return now.Add(-firstLookback)
	}
	if floor := now.Add(-maxLookback); last.Before(floor) {
		return floor
	}
	return last
}

// Check judges today's brief once it is due: journals the judgment and, when
// there is something to report, pushes it.
func (b *Briefer) Check() {
	if b.elsewhere() {
		return
	}
	now := b.cfg.Now()
	b.mu.Lock()
	last := b.last
	b.mu.Unlock()
	if !due(now, last.Day) {
		return
	}
	in := b.gather(windowStart(now, last.At), now)
	br := Brief{Day: now.Format(time.DateOnly), At: now, Since: in.Since, Counts: in.counts()}
	if br.Counts.total() > 0 {
		br.Sent = true
		br.Markdown = render(in)
	}
	b.record(br)
	b.mu.Lock()
	b.keep(br)
	b.mu.Unlock()
	if !br.Sent {
		slog.Info("brief: nothing to report", "day", br.Day)
		return
	}
	slog.Info("brief: sent", "day", br.Day, "meetings", br.Counts.Meetings, "replies", br.Counts.Replies,
		"deadlines", br.Counts.Deadlines, "done", br.Counts.Done, "failed", br.Counts.Failed,
		"waiting", br.Counts.Waiting, "errors", br.Counts.Errors, "tasks", br.Counts.Tasks,
		"planner", br.Counts.Planner, "routines", br.Counts.Routines, "notices", br.Counts.Notices,
		"custody", br.Counts.Custody, "midway", br.Counts.Midway)
	if b.cfg.Carried != nil && len(in.Notices) > 0 {
		ids := make([]string, 0, len(in.Notices))
		for _, it := range in.Notices {
			ids = append(ids, it.ID)
		}
		b.cfg.Carried(ids)
	}
	if b.cfg.Push != nil {
		b.cfg.Push(notify.Push{ID: "brief:" + br.Day, Kind: notify.KindNotice, Level: notify.LevelTimely,
			Title: "早报", Body: pushBody(br.Counts)})
	}
}

func (b *Briefer) record(br Brief) {
	if b.cfg.Journal == nil {
		return
	}
	data, err := json.Marshal(br)
	if err == nil {
		err = b.cfg.Journal.Append(journal.Entry{TS: br.At, Kind: journal.KindBrief, Unit: journalUnit, Data: data})
	}
	if err != nil {
		slog.Warn("brief: journal append failed", "day", br.Day, "err", err)
	}
}
