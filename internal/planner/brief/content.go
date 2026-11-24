package brief

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/custody"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/parked"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Rendering caps.
const (
	maxItems = 10 // per section; the rest become a count
	// maxFrontItems is how many open items the brief lists under a line.
	maxFrontItems = 3
	// maxPatrolNew is how many of the patrol's new findings are listed.
	maxPatrolNew = 3
	titleRunes   = 80
	// maxSlack is how many unread Slack conversations are listed: more than
	// a section's, as the digest is what the user reads them by.
	maxSlack       = 20
	slackNameRunes = 40
	// parkedRunes is what a 搁置区 line shows of its item, the source in it.
	parkedRunes = 160
	// maxPRLinks is how many PRs a finished session's line links; more are
	// counted.
	maxPRLinks = 3
	// frontIdle is how long a line of work goes unchanged before the brief
	// says so.
	frontIdle = 3 * 24 * time.Hour
	// maxTidy is how many sessions the morning tidy lists.
	maxTidy = 30
)

// ref is a unit as a brief line shows it.
type ref struct {
	Name string
	// Link is the dashboard's in-page link to the session, "" when the
	// dashboard cannot open the unit.
	Link string
	WS   string // workspace name
}

type doneItem struct {
	ref
	// Unit is the session's key.
	Unit  string
	At    time.Time
	Files int
	PRs   []string // PR URLs, in the order first seen
	// Recovered is set when the unit failed in the window and finished
	// after it; Retries are the retries custody sent it.
	Recovered bool
	Retries   int
}

type failedItem struct {
	ref
	At    time.Time
	Class health.ErrorClass
	// Stopped is set when custody gave up on the unit in the window.
	Stopped bool
}

type routineItem struct {
	ref
	At        time.Time
	FirstLine string
}

// custodyTally is what custody did in the window (its journal): Retries
// sent and the units they brought back (Recovered) or that it stopped on
// (Stopped), and the cards it answered as recommended.
type custodyTally struct {
	Retries, Recovered, Stopped, Answered int
}

func (c custodyTally) any() bool { return c.Retries+c.Stopped+c.Answered > 0 }

type inboxItem struct {
	ref
	ID      string
	Kind    notify.Kind
	Title   string
	Why     string
	Created time.Time
}

type taskItem struct {
	ref
	Kind  task.Kind
	State task.State
	Due   time.Time
	Since time.Time
}

type heldItem struct {
	Mention
	At time.Time
}

// input is what one brief is rendered from: the same input gives the same
// bytes.
type input struct {
	Now, Since time.Time
	Meetings   []meetingItem
	Replies    []replyItem
	Deadlines  []deadlineItem
	// StaleDeadlines counts the Asana tasks overdue past staleDays, folded
	// into one line.
	StaleDeadlines int
	// Slack are the unread Slack conversations as today's newest scout run
	// with a digest summed them up; SlackTotal counts all of them.
	Slack      []scout.SlackConversation
	SlackTotal int
	// Midway is when the Midway credential expires, set only when that is
	// before 09:00 today (or past).
	Midway time.Time
	// Fronts are the lines of work in progress, the latest updated first.
	Fronts []front.Front
	// Items are the open items of those lines, by line, oldest first.
	Items map[string][]front.Item
	// Projects is the project patrol's part.
	Projects ProjectPatrol
	// Done are the sessions handed out (the Planner's dispatches, another
	// node's) that finished; Yours the ones the user opened that got
	// somewhere, which the user was there for and the brief only names.
	Done    []doneItem
	Yours   []doneItem
	Failed  []failedItem
	Custody custodyTally
	// Routines are the cron jobs whose latest successful result changed;
	// Unchanged counts the ones whose did not.
	Routines  []routineItem
	Unchanged int
	// Waiting (questions, decisions) and Errors are the open items the
	// badge counts; the brief only counts them (D5).
	Waiting []inboxItem
	Errors  []inboxItem
	// Notices are the open notices the brief carries (§15.1, D5), closed
	// once it is sent.
	Notices []inboxItem
	Tasks   []taskItem
	Held    []heldItem
	// Parked is the 搁置区, set in the week's first brief only.
	Parked []parked.Entry
}

func (in input) counts() Counts {
	c := Counts{Meetings: len(in.Meetings), Replies: len(in.Replies), Deadlines: len(in.Deadlines),
		Done: len(in.Done), Yours: len(in.Yours), Fronts: len(in.Fronts), Failed: len(in.Failed), Waiting: len(in.Waiting), Errors: len(in.Errors),
		Tasks: len(in.Tasks), Planner: len(in.Held), Routines: len(in.Routines), Notices: len(in.Notices),
		Slack: max(in.SlackTotal, len(in.Slack)), Parked: len(in.Parked),
		Custody: in.Custody.Retries + in.Custody.Stopped + in.Custody.Answered}
	if !in.Midway.IsZero() {
		c.Midway = 1
	}
	p := in.Projects
	c.Patrol = len(p.PRs) + len(p.New) + len(p.Stopped)
	c.PatrolNews = len(p.New) + len(p.Stopped)
	for _, pr := range p.PRs {
		if pr.Fresh {
			c.PatrolNews++
		}
	}
	return c
}

// healthData is the part of a health journal entry's data the brief reads
// (monitor/health's journalData).
type healthData struct {
	From     health.State      `json:"from"`
	To       health.State      `json:"to"`
	Kind     health.Kind       `json:"kind"`
	ErrClass health.ErrorClass `json:"error_class"`
	Systemic bool              `json:"systemic"`
}

// seen is what the window shows of one unit.
type seen struct {
	ws, kind string
	doneAt   time.Time
	files    map[string]bool
	prs      []string
	failed   *healthData
	failedAt time.Time
	// failedTurn is the latest turn that ended in an error: one after
	// doneAt means the unit's last word is a failure, whether or not health
	// has confirmed it yet.
	failedTurn time.Time
	// What custody did to the unit: retries sent, and whether it stopped.
	retries int
	stopped bool
}

// gather collects the brief's items for the window since..now. Units
// without a workspace are left out (§3.8), but for the user's own sessions
// (health.UserSession: a chat started in the default directory belongs to
// none), listed by name alone; so is the Planner's own namespace. Finished means a turn
// ended with an answer (a local session's result frame, journaled once per
// turn however short; a remote unit's health transition from active to
// idle, the only sign of it here; or an output: a restart dates no turn), no
// turn failed after it, and the unit is not running, waiting or failed now;
// routine units (cron, daemons) and terminal sessions only count when they
// fail, and a dispatched session that failed or that the user stopped does
// not count. A unit that failed in the window is listed as failed while it
// is not healthy again; once it is, as finished if it finished after the
// failure, with what custody did for it.
func (b *Briefer) gather(since, now time.Time) input {
	in := input{Now: now, Since: since}
	var snap health.Snapshot
	if b.cfg.Health != nil {
		snap = b.cfg.Health()
	}
	cur := make(map[string]health.Unit, len(snap.Units))
	for _, u := range snap.Units {
		cur[u.ID] = u
	}
	units := map[string]*seen{}
	at := func(id, ws string, kind health.Kind) *seen {
		s := units[id]
		if s == nil {
			s = &seen{files: map[string]bool{}}
			units[id] = s
		}
		s.ws, s.kind = cmp.Or(ws, s.ws), cmp.Or(string(kind), s.kind)
		return s
	}
	held := map[string]heldItem{} // by key: the latest hold of a thing
	if b.cfg.Journal != nil {
		entries, err := b.cfg.Journal.Read(since, now.Add(time.Second))
		if err != nil {
			slog.Warn("brief: read journal", "err", err)
		}
		for _, e := range entries {
			switch e.Kind {
			case journal.KindNotify:
				var d holdData
				if json.Unmarshal(e.Data, &d) != nil || d.Event != eventHold || d.Title == "" {
					continue
				}
				held[cmp.Or(d.Key, d.Title)] = heldItem{Mention: d.Mention, At: e.TS}
			case journal.KindHealth:
				var d healthData
				if json.Unmarshal(e.Data, &d) != nil || d.Systemic {
					continue
				}
				switch {
				case d.To == health.Failed:
					s := at(e.Unit, e.WS, d.Kind)
					s.failed, s.failedAt = &d, e.TS
				case d.From == health.Active && d.To == health.Idle && d.Kind == health.KindRemote:
					// A local session's turns are counted from their result
					// frames (KindTurn): a reconcile samples once a minute,
					// misses a shorter turn and dates a longer one late.
					s := at(e.Unit, e.WS, d.Kind)
					s.doneAt = later(s.doneAt, e.TS)
				}
			case journal.KindTurn:
				var d activity.Turn
				if json.Unmarshal(e.Data, &d) != nil {
					continue
				}
				switch d.Outcome {
				case activity.TurnDone:
					s := at(e.Unit, e.WS, "")
					s.doneAt = later(s.doneAt, e.TS)
				case activity.TurnFailed:
					s := at(e.Unit, e.WS, "")
					s.failedTurn = later(s.failedTurn, e.TS)
				}
			case journal.KindActivity:
				var a activity.Activity
				if json.Unmarshal(e.Data, &a) != nil || !a.IsOutput() {
					continue
				}
				s := at(e.Unit, e.WS, "")
				s.doneAt = later(s.doneAt, e.TS)
				if a.Path != "" {
					s.files[a.Dir+"\x00"+a.Path] = true
				} else if !slices.Contains(s.prs, a.URL) {
					s.prs = append(s.prs, a.URL)
				}
			case journal.KindCustody:
				var d note.CustodyData
				if json.Unmarshal(e.Data, &d) != nil {
					continue
				}
				switch {
				case d.Event == "sent" && d.Reason == string(custody.ReasonRetry):
					at(e.Unit, e.WS, "").retries++
					in.Custody.Retries++
				case d.Event == "sent" && d.Reason == string(custody.ReasonSuggested):
					in.Custody.Answered++
				case d.Event == "stopped":
					if s := at(e.Unit, e.WS, ""); !s.stopped {
						s.stopped = true
						in.Custody.Stopped++
					}
				}
			}
		}
	}

	var tasks []task.Task
	if b.cfg.Tasks != nil {
		tasks = b.cfg.Tasks()
	}
	// The dispatched sessions that failed or that the user stopped did not
	// finish, whatever their turns did.
	unfinished := map[string]bool{}
	for _, t := range tasks {
		if t.Group == task.GroupYours {
			continue
		}
		if t.Kind == task.KindSession && (t.State == task.Failed || t.State == task.Done && t.By == task.ByUser && t.Why == task.StoppedWhy) {
			unfinished[t.ID] = true
		}
	}

	wsNames := b.workspaceNames()
	for id, s := range units {
		u, live := cur[id]
		ws, kind := s.ws, cmp.Or(health.Kind(s.kind), kindOf(id))
		if live {
			ws, kind = cmp.Or(u.Workspace, ws), u.Kind
		}
		if ws == "" && !health.UserSession(id, kind) || sessionkey.IsPlannerNSKey(id) {
			continue
		}
		r := b.ref(id, kind, cmp.Or(wsNames[ws], ws))
		recovered := s.failed != nil && live && (u.State == health.Active || u.State == health.Idle)
		if recovered && s.retries > 0 {
			in.Custody.Recovered++
		}
		if s.failed != nil && !recovered {
			in.Failed = append(in.Failed, failedItem{ref: r, At: s.failedAt, Class: s.failed.ErrClass, Stopped: s.stopped})
			continue
		}
		if s.doneAt.IsZero() || s.failedTurn.After(s.doneAt) || recovered && !s.doneAt.After(s.failedAt) || unfinished[id] ||
			kind == health.KindCron || kind == health.KindSysession || kind == health.KindExternal {
			continue
		}
		if live && u.State != health.Idle && u.State != health.Unknown {
			continue
		}
		d := doneItem{ref: r, Unit: id, At: s.doneAt, Files: len(s.files), PRs: s.prs, Recovered: recovered, Retries: s.retries}
		if kind == health.KindSession && !sessionkey.IsTaskKey(id) {
			in.Yours = append(in.Yours, d)
		} else {
			in.Done = append(in.Done, d)
		}
	}
	if b.cfg.Fronts != nil {
		in.Fronts = b.cfg.Fronts()
	}
	if b.cfg.Items != nil && len(in.Fronts) > 0 {
		in.Items = map[string][]front.Item{}
		for _, it := range b.cfg.Items() {
			if it.Front != "" {
				in.Items[it.Front] = append(in.Items[it.Front], it)
			}
		}
	}
	if b.cfg.Projects != nil {
		in.Projects = b.cfg.Projects(since)
	}

	if b.cfg.Inbox != nil {
		for _, it := range b.cfg.Inbox() {
			// What waits on the user, the errors and the notices the brief
			// carries: not the ones that close themselves (a condition, a
			// task done until the session is opened) nor a promise's
			// reminder, which 未完成的任务 lists.
			if it.State != notify.StateOpen || it.Kind == notify.KindNotice && (it.AutoClose || strings.HasPrefix(it.Source, "promise:")) {
				continue
			}
			kind := kindOf(it.Unit)
			if u, ok := cur[it.Unit]; ok {
				kind = u.Kind
			}
			r := ref{WS: cmp.Or(wsNames[it.WS], it.WS)}
			// The Planner's own items need no link: the brief is on its page.
			if it.Unit != "" && !sessionkey.IsPlannerNSKey(it.Unit) {
				r = b.ref(it.Unit, kind, r.WS)
			}
			item := inboxItem{ref: r, ID: it.ID, Kind: it.Kind, Title: it.Title, Why: it.Why, Created: it.Created}
			switch {
			case it.Kind.WaitsOnUser():
				in.Waiting = append(in.Waiting, item)
			case it.Kind == notify.KindNotice:
				in.Notices = append(in.Notices, item)
			default:
				in.Errors = append(in.Errors, item)
			}
		}
	}

	if b.cfg.Routines != nil {
		for _, rt := range b.cfg.Routines(since, now) {
			if !rt.Changed {
				in.Unchanged++
				continue
			}
			in.Routines = append(in.Routines, routineItem{ref: b.ref(rt.Unit, health.KindCron, ""), At: rt.At, FirstLine: rt.FirstLine})
		}
	}

	if b.cfg.Midway != nil {
		y, m, d := now.Date()
		if exp := b.cfg.Midway(); !exp.IsZero() && exp.Before(time.Date(y, m, d, midwayBy, 0, 0, 0, now.Location())) {
			in.Midway = exp
		}
	}

	// 未完成的任务, in 任务's order: the promises past due and not done
	// (§15.4), and the live dispatched sessions waiting on the user or
	// failed; one only in progress is not news. The sessions the user
	// opened (你开的) are not the brief's tasks: their 等你 and 失败 are in
	// the inbox already.
	if b.cfg.Tasks != nil {
		for _, t := range tasks {
			switch {
			case t.Group == task.GroupYours, t.State == task.Done || t.State == task.Remind:
				continue
			case t.Kind == task.KindPromise && (t.Due.IsZero() || t.Due.After(now)):
				continue
			case t.Kind == task.KindSession && (t.State == task.Doing || !t.Retired.IsZero()):
				continue
			}
			r := ref{Name: t.Title}
			if t.Kind == task.KindSession {
				r = b.ref(t.ID, health.KindSession, "")
				r.Name = t.Title
			}
			in.Tasks = append(in.Tasks, taskItem{ref: r, Kind: t.Kind, State: t.State, Due: t.Due, Since: t.Created})
		}
	}

	for _, h := range held {
		in.Held = append(in.Held, h)
	}
	slices.SortFunc(in.Held, func(x, y heldItem) int { return cmp.Or(x.At.Compare(y.At), cmp.Compare(x.Key, y.Key)) })

	b.gatherScout(&in)

	newest := func(t1, t2 time.Time, n1, n2 string) int {
		return cmp.Or(t2.Compare(t1), cmp.Compare(n1, n2))
	}
	slices.SortFunc(in.Done, func(x, y doneItem) int { return newest(x.At, y.At, x.Name, y.Name) })
	slices.SortFunc(in.Yours, func(x, y doneItem) int { return newest(x.At, y.At, x.Name, y.Name) })
	slices.SortFunc(in.Failed, func(x, y failedItem) int { return newest(x.At, y.At, x.Name, y.Name) })
	oldest := func(x, y inboxItem) int { return cmp.Or(x.Created.Compare(y.Created), cmp.Compare(x.Title, y.Title)) }
	slices.SortFunc(in.Waiting, oldest)
	slices.SortFunc(in.Errors, oldest)
	slices.SortFunc(in.Notices, oldest)
	slices.SortFunc(in.Routines, func(x, y routineItem) int { return newest(x.At, y.At, x.Name, y.Name) })
	return in
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (b *Briefer) workspaceNames() map[string]string {
	names := map[string]string{}
	if b.cfg.Workspaces != nil {
		for _, w := range b.cfg.Workspaces.List() {
			names[w.ID] = w.Name
		}
	}
	return names
}

// kindOf guesses a unit's kind from its ID when neither the snapshot nor the
// journal says (wireup/health_sources.go builds the IDs).
func kindOf(id string) health.Kind {
	switch {
	case sessionkey.IsCronKey(id):
		return health.KindCron
	case strings.HasPrefix(id, sessionkey.SysKeyPrefix):
		return health.KindSysession
	case strings.HasPrefix(id, "external:"):
		return health.KindExternal
	case strings.Contains(id, "/"):
		return health.KindRemote
	}
	return health.KindSession
}

// ref names unit id and links the sessions the dashboard can open: a local
// session or a remote node's, whose ID is node/key. Cron runs are not in the
// dashboard's sessions list, so they get no link.
func (b *Briefer) ref(id string, kind health.Kind, ws string) ref {
	name := ""
	if b.cfg.Name != nil {
		name = b.cfg.Name(id)
	}
	r := ref{Name: cmp.Or(name, id), WS: ws}
	q := url.Values{}
	switch kind {
	case health.KindSession:
		q.Set("session", id)
	case health.KindRemote:
		node, key, ok := strings.Cut(id, "/")
		if !ok {
			return r
		}
		q.Set("session", key)
		q.Set("node", node)
	default:
		return r
	}
	// Encode sorts by key, and "node" < "session"; the dashboard reads both
	// by name.
	r.Link = "#" + q.Encode()
	return r
}

var kindLabels = map[notify.Kind]string{
	notify.KindQuestion: "提问",
	notify.KindError:    "错误",
	notify.KindNotice:   "通知",
	notify.KindDecision: "决定",
}

var taskKinds = map[task.Kind]string{task.KindSession: "派出的会话", task.KindPromise: "承诺"}

var taskStates = map[task.State]string{task.Doing: "进行中", task.Waiting: "等你", task.Failed: "失败"}

// stamp is M/D HH:mm, as the dashboard writes a point in time.
const stamp = "1/2 15:04"

// render renders in as the brief's Markdown.
func render(in input) string {
	loc := in.Now.Location()
	c := in.counts()
	var b strings.Builder
	// No title: the card the dashboard shows it in already names the brief
	// and its day.
	// 等你 is one line, counted as the badge counts (D5): the items are in
	// 等你, the brief only says how many of each kind there are.
	waits := ""
	if n := c.Waiting + c.Errors; n > 0 {
		per := map[notify.Kind]int{notify.KindError: c.Errors}
		for _, it := range in.Waiting {
			per[it.Kind]++
		}
		var kinds []string
		for _, k := range []notify.Kind{notify.KindQuestion, notify.KindDecision, notify.KindError} {
			if per[k] > 0 {
				kinds = append(kinds, fmt.Sprintf("%s %d", kindLabels[k], per[k]))
			}
		}
		waits = fmt.Sprintf("等你 %d 条（%s），见「等你」", n, strings.Join(kinds, " · "))
	}
	if sum := summary(c, false); sum != "" {
		fmt.Fprintf(&b, "自 %s 以来：%s\n", in.Since.In(loc).Format(stamp), sum)
		if waits != "" {
			b.WriteString("\n" + waits + "\n")
		}
	} else {
		fmt.Fprintf(&b, "自 %s 以来：%s\n", in.Since.In(loc).Format(stamp), waits)
	}
	if !in.Midway.IsZero() {
		if exp := in.Midway.In(loc); exp.After(in.Now) {
			fmt.Fprintf(&b, "\nMidway 凭证 %s 到期，先 mwinit 再开始\n", exp.Format("15:04"))
		} else {
			fmt.Fprintf(&b, "\nMidway 凭证已于 %s 过期，先 mwinit 再开始\n", exp.Format(stamp))
		}
	}

	frontsSection(&b, in)
	patrolSection(&b, in.Projects)
	section(&b, "今天的日程", in.Meetings, func(m meetingItem) string {
		line := "全天"
		if !m.AllDay {
			line = m.Start.In(loc).Format("15:04") + "–" + m.End.In(loc).Format("15:04")
		}
		line += " · " + plain(textutil.TruncateRunes(m.Title, titleRunes))
		if m.Conflict {
			line += " · 冲突"
		}
		return line
	})
	section(&b, "在等你回复的", in.Replies, func(r replyItem) string {
		line := cmp.Or(plain(r.From), "某人") + " · " + plain(textutil.TruncateRunes(r.Title, titleRunes))
		if !r.At.IsZero() && r.At.Before(in.Now) {
			line += " · 已等 " + ago(in.Now.Sub(r.At))
		}
		return line
	})
	const asana = "今天到期或已过期的 Asana 任务"
	section(&b, asana, in.Deadlines, func(d deadlineItem) string {
		return plain(textutil.TruncateRunes(d.Title, titleRunes)) + " · " + dueLabel(d.Due, in.Now)
	})
	if n := in.StaleDeadlines; n > 0 && len(in.Deadlines) > 0 {
		fmt.Fprintf(&b, "- 另有 %d 条过期超过 %d 天\n", n, staleDays)
	} else if n > 0 {
		fmt.Fprintf(&b, "\n### %s\n- %d 条过期超过 %d 天\n", asana, n, staleDays)
	}

	if len(in.Slack) > 0 {
		fmt.Fprintf(&b, "\n### Slack 未读（%d）\n", c.Slack)
		for i, cv := range in.Slack {
			if i == maxSlack {
				break
			}
			b.WriteString("- " + slackLine(cv) + "\n")
		}
		if n := c.Slack - min(len(in.Slack), maxSlack); n > 0 {
			fmt.Fprintf(&b, "- …另有 %d 个会话\n", n)
		}
	}

	doneLine := func(d doneItem) string {
		line := link(d.Name, d.Link) + wsPart(d.WS) + " · " + d.At.In(loc).Format(stamp)
		var outs []string
		if d.Files > 0 {
			outs = append(outs, fmt.Sprintf("%d 个文件", d.Files))
		}
		if len(d.PRs) > maxPRLinks {
			// A session that opened a dozen PRs is one line, not a dozen links.
			outs = append(outs, fmt.Sprintf("%d 个 PR", len(d.PRs)))
		} else {
			for _, u := range d.PRs {
				outs = append(outs, link(prLabel(u), u))
			}
		}
		if len(outs) > 0 {
			line += " · 产出 " + strings.Join(outs, "、")
		}
		switch {
		case d.Recovered && d.Retries > 0:
			line += fmt.Sprintf(" · 托管重试 %d 次后恢复", d.Retries)
		case d.Recovered:
			line += " · 出错后已恢复"
		}
		return line
	}
	section(&b, "完成的", in.Done, doneLine)
	// The user was in these, so they come after what was handed out.
	section(&b, "你开的会话有新输出", in.Yours, doneLine)
	section(&b, "失败的", in.Failed, func(f failedItem) string {
		line := link(f.Name, f.Link) + wsPart(f.WS) + " · " + f.At.In(loc).Format(stamp)
		switch {
		case f.Class == health.ErrRateLimit:
			line += " · 限流，托管不重试"
		case f.Class != health.ErrNone && f.Class != health.ErrOther:
			line += " · " + notify.ClassLabel(f.Class)
		}
		if f.Stopped {
			line += " · 托管已停"
		}
		return line
	})
	if c := in.Custody; c.any() {
		var parts []string
		if c.Retries > 0 {
			parts = append(parts, fmt.Sprintf("重试 %d 次（恢复 %d、停下 %d）", c.Retries, c.Recovered, c.Stopped))
		} else if c.Stopped > 0 {
			parts = append(parts, fmt.Sprintf("停下 %d 个", c.Stopped))
		}
		if c.Answered > 0 {
			parts = append(parts, fmt.Sprintf("代答 %d 次", c.Answered))
		}
		fmt.Fprintf(&b, "\n托管：%s\n", strings.Join(parts, "、"))
	}
	section(&b, "例行任务有变化", in.Routines, func(r routineItem) string {
		line := link(r.Name, r.Link) + " · " + r.At.In(loc).Format(stamp)
		if r.FirstLine != "" {
			line += " · " + plain(r.FirstLine)
		}
		return line
	})
	if n := in.Unchanged; n > 0 && len(in.Routines) > 0 {
		fmt.Fprintf(&b, "- 另有 %d 个例行任务结果没变\n", n)
	} else if n > 0 {
		fmt.Fprintf(&b, "\n例行任务 %d 个，结果都没变\n", n)
	}
	section(&b, "通知", in.Notices, func(it inboxItem) string {
		line := link(textutil.TruncateRunes(it.Title, titleRunes), it.Link)
		if it.Why != "" {
			line += " · " + plain(it.Why)
		}
		return line + wsPart(it.WS) + " · " + it.Created.In(loc).Format(stamp)
	})
	section(&b, "未完成的任务", in.Tasks, func(t taskItem) string {
		line := link(textutil.TruncateRunes(t.Name, titleRunes), t.Link) + " · " + taskKinds[t.Kind] + " · " + taskStates[t.State]
		if !t.Due.IsZero() {
			line += " · 到期 " + t.Due.In(loc).Format(stamp)
		}
		return line + " · 已 " + ago(in.Now.Sub(t.Since))
	})
	section(&b, "Planner 记下的", in.Held, func(h heldItem) string {
		line := link(textutil.TruncateRunes(h.Title, titleRunes), escapeHref(h.Link))
		if h.Why != "" {
			line += " · " + plain(h.Why)
		}
		return line + " · " + h.At.In(loc).Format(stamp)
	})
	section(&b, "搁置区", in.Parked, func(e parked.Entry) string {
		line := plain(textutil.TruncateRunes(e.Text, parkedRunes))
		if d, err := time.ParseInLocation(time.DateOnly, e.Day, loc); err == nil {
			// Counted in days: an item moves in by the day, not the hour.
			y, m, dd := in.Now.In(loc).Date()
			if n := int(time.Date(y, m, dd, 0, 0, 0, 0, loc).Sub(d).Hours()/24 + 0.5); n > 0 {
				line += fmt.Sprintf(" · 已搁置 %d 天", n)
			} else {
				line += " · 今天搁置"
			}
		}
		if e.ByUser {
			line += " · 你说放下"
		}
		return line
	})
	return b.String()
}

// slackKinds label a conversation the way Slack's sidebar does.
var slackKinds = map[string]string{scout.SlackDM: "私信 ", scout.SlackGroup: "群私信 ", scout.SlackChannel: "#",
	scout.SlackThread: "线程 "}

// slackLine is one unread conversation: where, how often the user was
// mentioned, what it is about.
func slackLine(cv scout.SlackConversation) string {
	line := slackKinds[cv.Kind] + plain(textutil.TruncateRunes(cv.Name, slackNameRunes))
	if cv.Mentions > 0 {
		line += fmt.Sprintf(" · @你 %d", cv.Mentions)
	}
	if cv.Summary != "" {
		line += " · " + plain(textutil.TruncateRunes(cv.Summary, titleRunes))
	}
	return line
}

// frontsSection lists the lines of work in progress, each with the first
// of its open items under it (§15.23, D143): what is left on a line is what
// the user picks the day's work from. The items under no line are not
// listed: the Planner page's left column has them.
func frontsSection(b *strings.Builder, in input) {
	if len(in.Fronts) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### 工作线（%d）\n", len(in.Fronts))
	for i, f := range in.Fronts {
		if i == maxItems {
			fmt.Fprintf(b, "- …另有 %d 项\n", len(in.Fronts)-i)
			break
		}
		line := "**" + plain(f.Name) + "**"
		if f.State != "" {
			line += "：" + plain(f.State)
		}
		if f.Next != "" {
			line += " → " + plain(f.Next)
		}
		if w := strings.TrimSpace(strings.TrimPrefix(f.Waiting, "等")); w != "" {
			line += "（等 " + plain(w) + "）"
		}
		if idle := in.Now.Sub(f.Updated); !f.Updated.IsZero() && idle >= frontIdle {
			line += " · " + ago(idle) + "没动"
		}
		b.WriteString("- " + line + "\n")
		for j, it := range in.Items[f.ID] {
			if j == maxFrontItems {
				fmt.Fprintf(b, "  - …另有 %d 条\n", len(in.Items[f.ID])-j)
				break
			}
			b.WriteString("  - 未了：" + plain(it.Text) + "\n")
		}
	}
}

// patrolSection is the project patrol's part (§15.23 ⑤), after the lines
// of work: the pull requests it had made that wait on the user, each with
// its link and checks; the new findings, the first few; and, once, a kind
// it stopped reporting. Nothing at all with none of them.
func patrolSection(b *strings.Builder, p ProjectPatrol) {
	n := len(p.PRs) + len(p.New) + len(p.Stopped)
	if n == 0 {
		return
	}
	fmt.Fprintf(b, "\n### 巡查（%d）\n", n)
	for i, pr := range p.PRs {
		if i == maxItems {
			fmt.Fprintf(b, "- …另有 %d 个 PR\n", len(p.PRs)-i)
			break
		}
		line := "已备好的 PR：" + link(textutil.TruncateRunes(pr.Title, titleRunes), escapeHref(pr.URL))
		if pr.CI != "" {
			line += " · CI " + plain(pr.CI)
		}
		b.WriteString("- " + line + wsPart(pr.Workspace) + "\n")
	}
	for i, f := range p.New {
		if i == maxPatrolNew {
			fmt.Fprintf(b, "- …另有 %d 条\n", len(p.New)-i)
			break
		}
		b.WriteString("- 新发现：" + plain(textutil.TruncateRunes(f.Title, titleRunes)) + " · " + plain(f.Kind) + wsPart(f.Workspace) + "\n")
	}
	for _, s := range p.Stopped {
		fmt.Fprintf(b, "- %s 类你处置过的最近 %d 条只采纳了 %d 条，已停报；要恢复，从 planner/patrol/declined.json 的 kinds 里删掉它\n", plain(s.Kind), s.Of, s.Adopted)
	}
}

func section[T any](b *strings.Builder, title string, items []T, line func(T) string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### %s（%d）\n", title, len(items))
	for i, it := range items {
		if i == maxItems {
			fmt.Fprintf(b, "- …另有 %d 项\n", len(items)-i)
			break
		}
		b.WriteString("- " + line(it) + "\n")
	}
}

// summary is the counts as one line, the zero ones left out. The brief
// gives 等你 a line of its own; the push, its text only this line, counts it
// here (questions, decisions and errors, as the badge does).
func summary(c Counts, push bool) string {
	waits := 0
	if push {
		waits = c.Waiting + c.Errors
	}
	var parts []string
	for _, p := range []struct {
		label string
		n     int
	}{{"工作线", c.Fronts}, {"巡查", c.Patrol}, {"今天日程", c.Meetings}, {"等你回复", c.Replies}, {"Asana 到期", c.Deadlines}, {"Slack 未读", c.Slack},
		{"完成", c.Done}, {"你开的会话有新输出", c.Yours}, {"失败", c.Failed}, {"例行任务有变化", c.Routines}, {"等你", waits},
		{"通知", c.Notices}, {"未完成的任务", c.Tasks}, {"Planner 记下", c.Planner}, {"搁置", c.Parked}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", p.label, p.n))
		}
	}
	return strings.Join(parts, " · ")
}

// pushBody is the notification's text: the counts, nothing from the items.
func pushBody(c Counts) string { return summary(c, true) }

func link(text, href string) string {
	text = plain(text)
	if href == "" {
		return text
	}
	return "[" + text + "](" + href + ")"
}

// tidyText is the morning tidy's block (§15.18): the sessions that got
// somewhere or failed since the last brief, by key, for the Planner to
// bring the ledger up to date from. "" with none.
func tidyText(in input) string {
	type row struct {
		unit, name, ws, what string
		at                   time.Time
	}
	var rows []row
	for _, d := range in.Yours {
		rows = append(rows, row{d.Unit, d.Name, d.WS, "你开的", d.At})
	}
	for _, d := range in.Done {
		rows = append(rows, row{d.Unit, d.Name, d.WS, "派出的", d.At})
	}
	if len(rows) == 0 {
		return ""
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Or(b.at.Compare(a.at), cmp.Compare(a.unit, b.unit)) })
	loc := in.Now.Location()
	var b strings.Builder
	fmt.Fprintf(&b, "<naozhi-morning day=%q>\n早报之前先整理工作线台账。自上次早报（%s）以来有新输出的会话（%d 个）：\n",
		in.Now.Format(time.DateOnly), in.Since.In(loc).Format("01-02 15:04"), len(rows))
	for i, r := range rows {
		if i == maxTidy {
			fmt.Fprintf(&b, "- …另有 %d 个，见 index.json\n", len(rows)-i)
			break
		}
		line := "- " + r.unit
		if r.name != "" && r.name != r.unit {
			line += "（" + plain(textutil.TruncateRunes(r.name, titleRunes)) + "）"
		}
		if r.ws != "" {
			line += " · 工作区 " + plain(r.ws)
		}
		b.WriteString(line + " · " + r.what + " · 最后输出 " + r.at.In(loc).Format("01-02 15:04") + "\n")
	}
	b.WriteString("对照快照里的工作线逐个看：它属于哪条线、有没有新进展或新决定（先读它的摘要，index.json 里有路径；不够再分段读事件日志），" +
		"用 front 块更新那条线的现状、下一步、等谁和 sessions；看不出属于哪条线、但看起来会持续几天的事建一条新线；闲聊、一次性问答不建线。" +
		"几天没动、看起来已经结束的线先不要关，在回复里提一句。回复只写两三行，说改了哪几条；早报由网关接着发，不要用 notify 重复。\n</naozhi-morning>")
	return b.String()
}

func wsPart(ws string) string {
	if ws == "" {
		return ""
	}
	return " · 工作区 " + plain(ws)
}

// hrefEscapes keep a link's URL inside the Markdown link's parentheses.
var hrefEscapes = strings.NewReplacer("(", "%28", ")", "%29", " ", "%20", "<", "%3C", ">", "%3E")

func escapeHref(u string) string { return hrefEscapes.Replace(u) }

var prNumber = regexp.MustCompile(`/pull/(\d+)`)

func prLabel(u string) string {
	if m := prNumber.FindStringSubmatch(u); m != nil {
		return "PR #" + m[1]
	}
	return "PR"
}

// markdownish are the runes a name or title could use to open a link, code,
// emphasis, math (the dashboard renders $…$ and \[…\] with KaTeX) or a
// table cell; plain swaps them for look-alikes rather than escaping, since a
// backslash itself opens math there.
var markdownish = strings.NewReplacer(
	"[", "［", "]", "］", "`", "'", "*", "＊", "$", "＄", "\\", "＼", "<", "‹", ">", "›", "|", "｜",
)

// plain makes s one line of literal text.
func plain(s string) string {
	return markdownish.Replace(strings.Join(strings.Fields(s), " "))
}

// ago is a wait as the user reads it.
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", max(int(d/time.Minute), 1))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小时", int(d/time.Hour))
	}
	return fmt.Sprintf("%d 天", int(d/(24*time.Hour)))
}
