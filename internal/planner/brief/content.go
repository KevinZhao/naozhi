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
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Rendering caps.
const (
	maxItems   = 10 // per section; the rest become a count
	titleRunes = 80
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
	// Midway is when the Midway credential expires, set only when that is
	// before 09:00 today (or past).
	Midway  time.Time
	Done    []doneItem
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
}

func (in input) counts() Counts {
	c := Counts{Meetings: len(in.Meetings), Replies: len(in.Replies), Deadlines: len(in.Deadlines),
		Done: len(in.Done), Failed: len(in.Failed), Waiting: len(in.Waiting), Errors: len(in.Errors),
		Tasks: len(in.Tasks), Planner: len(in.Held), Routines: len(in.Routines), Notices: len(in.Notices),
		Custody: in.Custody.Retries + in.Custody.Stopped + in.Custody.Answered}
	if !in.Midway.IsZero() {
		c.Midway = 1
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
	// healedAt is the latest Failed→Idle: after custody's retry, the
	// retried turn ending, which a turn too short for a reconcile to see
	// active records with no Active→Idle.
	healedAt time.Time
	// What custody did to the unit: retries sent, and whether it stopped.
	retries int
	stopped bool
}

// gather collects the brief's items for the window since..now. Units
// without a workspace are left out, as everywhere the gateway reports
// (§3.8), and so is the Planner's own namespace. Finished means a turn
// ended (a health transition from active to idle, or an output: a restart
// dates no turn) and the unit is not running, waiting or failed now;
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
				case d.From == health.Active && d.To == health.Idle:
					s := at(e.Unit, e.WS, d.Kind)
					s.doneAt = later(s.doneAt, e.TS)
				case d.From == health.Failed && d.To == health.Idle:
					s := at(e.Unit, e.WS, d.Kind)
					s.healedAt = later(s.healedAt, e.TS)
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
		if ws == "" || sessionkey.IsPlannerNSKey(id) {
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
		if recovered && s.retries > 0 && s.healedAt.After(s.failedAt) {
			s.doneAt = later(s.doneAt, s.healedAt)
		}
		if s.doneAt.IsZero() || recovered && !s.doneAt.After(s.failedAt) || unfinished[id] ||
			kind == health.KindCron || kind == health.KindSysession || kind == health.KindExternal {
			continue
		}
		if live && u.State != health.Idle && u.State != health.Unknown {
			continue
		}
		in.Done = append(in.Done, doneItem{ref: r, At: s.doneAt, Files: len(s.files), PRs: s.prs, Recovered: recovered, Retries: s.retries})
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
	// failed; one only in progress is not news.
	if b.cfg.Tasks != nil {
		for _, t := range tasks {
			switch {
			case t.State == task.Done || t.State == task.Remind:
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

	section(&b, "完成的", in.Done, func(d doneItem) string {
		line := link(d.Name, d.Link) + wsPart(d.WS) + " · " + d.At.In(loc).Format(stamp)
		var outs []string
		if d.Files > 0 {
			outs = append(outs, fmt.Sprintf("%d 个文件", d.Files))
		}
		for _, u := range d.PRs {
			outs = append(outs, link(prLabel(u), u))
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
	})
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
	return b.String()
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
	}{{"今天日程", c.Meetings}, {"等你回复", c.Replies}, {"Asana 到期", c.Deadlines},
		{"完成", c.Done}, {"失败", c.Failed}, {"例行任务有变化", c.Routines}, {"等你", waits},
		{"通知", c.Notices}, {"未完成的任务", c.Tasks}, {"Planner 记下", c.Planner}} {
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
