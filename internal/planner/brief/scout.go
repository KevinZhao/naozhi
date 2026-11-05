package brief

import (
	"fmt"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/planner/scout"
)

// allDay is how long an event runs to count as all-day: shown as 全天 and
// never in conflict.
const allDay = 24 * time.Hour

type meetingItem struct {
	Title      string
	Start, End time.Time
	AllDay     bool
	// Conflict is set when it overlaps another of today's meetings.
	Conflict bool
}

// replyItem is someone waiting on the user's reply, as the brief shows
// them: who, about what, since when; nothing of the message itself.
type replyItem struct {
	From, Title string
	At          time.Time
}

// staleDays is how many days overdue an Asana task may be to be listed; the
// older ones are only counted (D90).
const staleDays = 7

type deadlineItem struct {
	Title string
	Due   time.Time // zero when the scout gave none it could parse
	Link  string
}

// gatherScout adds what the scout knows: today's meetings not over, from
// the calendar cache; and, from every run that went through in the window,
// the people waiting on a reply and the Asana tasks due today or overdue.
// A run reports only what came after its cursor, so the replies waiting
// overnight are in the morning's first run, not the newest; and nothing
// tells a reply sent: these are the ones reported since the last brief.
func (b *Briefer) gatherScout(in *input) {
	now := in.Now
	if b.cfg.Calendar != nil {
		y, m, d := now.Date()
		tomorrow := time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
		for _, e := range b.cfg.Calendar() {
			if e.End.After(now) && e.Start.Before(tomorrow) {
				in.Meetings = append(in.Meetings, meetingItem{Title: e.Title, Start: e.Start, End: e.End,
					AllDay: e.End.Sub(e.Start) >= allDay})
			}
		}
		slices.SortStableFunc(in.Meetings, func(x, y meetingItem) int { return x.Start.Compare(y.Start) })
		for i := range in.Meetings {
			for j := i + 1; j < len(in.Meetings); j++ {
				x, y := &in.Meetings[i], &in.Meetings[j]
				if !x.AllDay && !y.AllDay && x.Start.Before(y.End) && y.Start.Before(x.End) {
					x.Conflict, y.Conflict = true, true
				}
			}
		}
	}
	if b.cfg.Patrol == nil {
		return
	}
	// The same finding from several runs is listed once, as the newest run
	// reported it.
	index := map[string]int{}
	var findings []scout.Finding
	for _, r := range b.cfg.Patrol(in.Since) {
		if r.Ended.After(now) {
			continue
		}
		for _, f := range r.Findings {
			k := f.Source + "\x00" + f.ID + "\x00" + f.Kind
			if i, ok := index[k]; ok {
				findings[i] = f.Finding
				continue
			}
			index[k] = len(findings)
			findings = append(findings, f.Finding)
		}
	}
	asana := map[string]bool{} // the links of the Asana tasks reported due
	for _, f := range findings {
		switch {
		case f.Kind == scout.KindReplyNeeded:
			at, _ := time.Parse(time.RFC3339, f.At)
			in.Replies = append(in.Replies, replyItem{From: f.From, Title: f.Title, At: at})
		case f.Kind == scout.KindDeadline && f.Source == scout.SourceAsana:
			due := parseDue(f.Due, now.Location())
			days := overdueDays(due, now)
			switch {
			case days < 0:
				continue
			case days > staleDays:
				in.StaleDeadlines++
			default:
				in.Deadlines = append(in.Deadlines, deadlineItem{Title: f.Title, Due: due, Link: f.Link})
			}
			if f.Link != "" {
				asana[f.Link] = true
			}
		}
	}
	// What the Planner held about an Asana task the section already
	// reports is not listed twice.
	in.Held = slices.DeleteFunc(in.Held, func(h heldItem) bool { return h.Link != "" && asana[h.Link] })
	// The longest wait first; the most overdue first; the undated last.
	slices.SortStableFunc(in.Replies, func(x, y replyItem) int { return earliest(x.At, y.At) })
	slices.SortStableFunc(in.Deadlines, func(x, y deadlineItem) int { return earliest(x.Due, y.Due) })
}

// earliest orders times earliest first, the zero time last.
func earliest(a, b time.Time) int {
	switch {
	case a.IsZero() == b.IsZero():
		return a.Compare(b)
	case a.IsZero():
		return 1
	}
	return -1
}

// parseDue reads a due time as the scout writes it: RFC 3339, or a bare
// date, local. Zero when it is neither.
func parseDue(s string, loc *time.Location) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc)
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, loc); err == nil {
		return t
	}
	return time.Time{}
}

// overdueDays is how many calendar days before now's a task fell due: 0
// today or with no due time, negative when it is not due yet.
func overdueDays(due, now time.Time) int {
	if due.IsZero() {
		return 0
	}
	y, m, d := due.Date()
	ny, nm, nd := now.Date()
	return int(time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC).Sub(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)) / (24 * time.Hour))
}

// dueLabel says when a task fell due, by calendar day.
func dueLabel(due, now time.Time) string {
	if due.IsZero() {
		return "到期"
	}
	days := overdueDays(due, now)
	if days <= 0 {
		return "今天到期"
	}
	return fmt.Sprintf("已过期 %d 天", days)
}
