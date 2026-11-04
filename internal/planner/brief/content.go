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
	"github.com/naozhi/naozhi/internal/planner/journal"
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
}

type failedItem struct {
	ref
	At        time.Time
	Class     health.ErrorClass
	Recovered bool // the unit is active or idle again
}

type inboxItem struct {
	ref
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

// input is what one brief is rendered from: the same input gives the same
// bytes.
type input struct {
	Now, Since time.Time
	Done       []doneItem
	Failed     []failedItem
	Waiting    []inboxItem
	Errors     []inboxItem
	Notices    []inboxItem
	Tasks      []taskItem
}

func (in input) counts() Counts {
	return Counts{Done: len(in.Done), Failed: len(in.Failed), Waiting: len(in.Waiting), Errors: len(in.Errors),
		Notices: len(in.Notices), Tasks: len(in.Tasks)}
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
}

// gather collects the brief's items for the window since..now. Units
// without a workspace are left out, as everywhere the gateway reports
// (§3.8), and so is the Planner's own namespace. Finished means a turn
// ended (a health transition from active to idle, an output, or an idle
// unit last active in the window) and the unit is not running, waiting or
// failed now; routine units (cron, daemons) and terminal sessions only
// count when they fail. A unit that failed in the window is listed as
// failed only.
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
	if b.cfg.Journal != nil {
		entries, err := b.cfg.Journal.Read(since, now.Add(time.Second))
		if err != nil {
			slog.Warn("brief: read journal", "err", err)
		}
		for _, e := range entries {
			switch e.Kind {
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
			}
		}
	}
	for _, u := range snap.Units {
		if u.State == health.Idle && !u.LastActive.Before(since) && !u.LastActive.After(now) {
			s := at(u.ID, u.Workspace, u.Kind)
			s.doneAt = later(s.doneAt, u.LastActive)
		}
	}

	wsNames := b.workspaceNames()
	failedUnits := map[string]bool{}
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
		if s.failed != nil {
			failedUnits[id] = true
			in.Failed = append(in.Failed, failedItem{ref: r, At: s.failedAt, Class: s.failed.ErrClass,
				Recovered: live && (u.State == health.Active || u.State == health.Idle)})
			continue
		}
		if s.doneAt.IsZero() || kind == health.KindCron || kind == health.KindSysession || kind == health.KindExternal {
			continue
		}
		if live && u.State != health.Idle && u.State != health.Unknown {
			continue
		}
		in.Done = append(in.Done, doneItem{ref: r, At: s.doneAt, Files: len(s.files), PRs: s.prs})
	}

	if b.cfg.Inbox != nil {
		for _, it := range b.cfg.Inbox() {
			if it.State != notify.StateOpen {
				continue
			}
			kind := kindOf(it.Unit)
			if u, ok := cur[it.Unit]; ok {
				kind = u.Kind
			}
			r := ref{WS: cmp.Or(wsNames[it.WS], it.WS)}
			if it.Unit != "" {
				r = b.ref(it.Unit, kind, r.WS)
			}
			item := inboxItem{ref: r, Kind: it.Kind, Title: it.Title, Why: it.Why, Created: it.Created}
			switch {
			case it.Kind.WaitsOnUser():
				in.Waiting = append(in.Waiting, item)
			case it.Kind != notify.KindError:
				in.Notices = append(in.Notices, item)
			case it.Unit != "" && failedUnits[it.Unit] && it.Source == "failed:"+it.Unit:
				// Already listed under 失败的.
			default:
				in.Errors = append(in.Errors, item)
			}
		}
	}

	// 未完成的任务, in 任务's order: the promises past due and not done
	// (§15.4), and the live dispatched sessions waiting on the user or
	// failed; one only in progress is not news.
	if b.cfg.Tasks != nil {
		for _, t := range b.cfg.Tasks() {
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

	newest := func(t1, t2 time.Time, n1, n2 string) int {
		return cmp.Or(t2.Compare(t1), cmp.Compare(n1, n2))
	}
	slices.SortFunc(in.Done, func(x, y doneItem) int { return newest(x.At, y.At, x.Name, y.Name) })
	slices.SortFunc(in.Failed, func(x, y failedItem) int { return newest(x.At, y.At, x.Name, y.Name) })
	oldest := func(x, y inboxItem) int { return cmp.Or(x.Created.Compare(y.Created), cmp.Compare(x.Title, y.Title)) }
	slices.SortFunc(in.Waiting, oldest)
	slices.SortFunc(in.Errors, oldest)
	slices.SortFunc(in.Notices, oldest)
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

var weekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

const stamp = "01-02 15:04"

// render renders in as the brief's Markdown.
func render(in input) string {
	loc := in.Now.Location()
	c := in.counts()
	var b strings.Builder
	fmt.Fprintf(&b, "## 早报 · %d月%d日 %s\n\n", in.Now.Month(), in.Now.Day(), weekdays[in.Now.Weekday()])
	fmt.Fprintf(&b, "自 %s 以来：%s\n", in.Since.In(loc).Format(stamp), summary(c, "未关闭的错误"))

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
		return line
	})
	section(&b, "失败的", in.Failed, func(f failedItem) string {
		line := link(f.Name, f.Link) + wsPart(f.WS) + " · " + f.At.In(loc).Format(stamp)
		if f.Class != "" && f.Class != health.ErrNone {
			line += " · " + notify.ClassLabel(f.Class)
		}
		if f.Recovered {
			line += " · 已恢复"
		}
		return line
	})
	item := func(it inboxItem) string {
		line := cmp.Or(kindLabels[it.Kind], string(it.Kind)) + " · " + link(textutil.TruncateRunes(it.Title, titleRunes), it.Link)
		if it.Why != "" {
			line += " · " + plain(it.Why)
		}
		return line + wsPart(it.WS) + " · 已等 " + ago(in.Now.Sub(it.Created))
	}
	section(&b, "仍在等你的", in.Waiting, item)
	section(&b, "未关闭的错误", in.Errors, item)
	section(&b, "通知", in.Notices, item)
	section(&b, "未完成的任务", in.Tasks, func(t taskItem) string {
		line := link(textutil.TruncateRunes(t.Name, titleRunes), t.Link) + " · " + taskKinds[t.Kind] + " · " + taskStates[t.State]
		if !t.Due.IsZero() {
			line += " · 到期 " + t.Due.In(loc).Format(stamp)
		}
		return line + " · 已 " + ago(in.Now.Sub(t.Since))
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

// summary is the counts as one line, the zero ones left out; errors names
// the error count.
func summary(c Counts, errors string) string {
	var parts []string
	for _, p := range []struct {
		label string
		n     int
	}{{"完成", c.Done}, {"失败", c.Failed}, {"等你", c.Waiting}, {errors, c.Errors}, {"通知", c.Notices}, {"未完成的任务", c.Tasks}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", p.label, p.n))
		}
	}
	return strings.Join(parts, " · ")
}

// pushBody is the notification's text: the counts, nothing from the items.
func pushBody(c Counts) string { return summary(c, "错误") }

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
