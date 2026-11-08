package agent

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Snapshot budget: the whole block stays near 2k tokens, so the Planner's
// context grows by a bounded amount per message whatever the fleet size.
// Every open promise's ID is listed past it all the same: the Planner closes
// a promise by its ID, and has no other place to read it from.
const (
	snapshotMaxRunes  = 2000
	snapshotLineRunes = 160
	snapshotMaxItems  = 10 // in the waiting section
	snapshotMaxUnits  = 20
	cutNoticeRunes    = 32
	// promiseRunes is what the promise lines take before their texts are
	// cut, to no less than promiseTextRunes each.
	promiseRunes     = snapshotMaxRunes / 2
	promiseTextRunes = 12
	// recentWindow is how long an ended unit still counts as "recently ended".
	recentWindow = 24 * time.Hour
)

// Waiting is one open "等你" item (docs/rfc/planner.md §15.1, PR ④).
type Waiting struct {
	Kind    string // 提问 | 错误 | 通知
	Title   string
	Unit    string
	Created time.Time
}

// Promise is one open promise the Planner made (PR ⑫); ID is what its
// status block names.
type Promise struct {
	ID   string
	Text string
	Due  time.Time // zero when it has no due time
}

// snapshotInput is what one snapshot is rendered from. A nil Waiting or
// Promises means the source is not wired, and its section is left out
// rather than reported empty.
type snapshotInput struct {
	Now      time.Time
	Waiting  []Waiting
	Promises []Promise
	Units    []health.Unit
	// Label is a unit's title, Name its cross-session name, Workspace a
	// workspace ID's name; any may return "".
	Label     func(id string) string
	Name      func(id string) string
	Workspace func(id string) string
}

var stateNames = map[health.State]string{
	health.Active:  "活跃",
	health.Idle:    "空闲",
	health.Waiting: "待你回复",
	health.Stalled: "停滞",
	health.Failed:  "失败",
	health.Lost:    "失联",
	health.Unknown: "未知",
}

// running states are listed as "在跑"; the rest only while recent.
func running(s health.State) bool {
	return s == health.Active || s == health.Waiting || s == health.Stalled
}

// renderSnapshot renders the deterministic <naozhi-snapshot> block the
// gateway puts before each user message: the same input gives the same
// bytes. Over the budget, trailing lines give way to a count, save the
// promise lines, which are always kept.
func renderSnapshot(in snapshotInput) string {
	full, _ := renderSnapshotKey(in)
	return full
}

// renderSnapshotKey is renderSnapshot and what of it a later one is
// compared with: the lines it shows without the times, the sessions' in
// any order. A running session's 最后输出 moves with its events, and its
// place in the list with it, which says nothing the Planner acts on; a
// state, a session coming or going, or which sessions fit is a change.
func renderSnapshotKey(in snapshotInput) (full, key string) {
	var lines []string
	// times[i] is lines[i]'s trailing time, left out of the key.
	var times []string
	keepFrom, keepTo := 0, 0 // the promise section, lines[keepFrom:keepTo]
	if in.Waiting != nil {
		items := slices.Clone(in.Waiting)
		slices.SortStableFunc(items, func(a, b Waiting) int { return a.Created.Compare(b.Created) })
		lines = append(lines, fmt.Sprintf("等你（%d）：", len(items)))
		for i, w := range items {
			if i == snapshotMaxItems {
				lines = append(lines, fmt.Sprintf("- …另有 %d 条", len(items)-i))
				break
			}
			line := "- [" + w.Kind + "] " + w.Title
			if w.Unit != "" {
				line += "（" + w.Unit + "）"
			}
			lines = append(lines, line)
		}
	}
	if in.Promises != nil {
		items := slices.Clone(in.Promises)
		slices.SortStableFunc(items, func(a, b Promise) int {
			// Undated promises after dated ones.
			if a.Due.IsZero() != b.Due.IsZero() {
				if a.Due.IsZero() {
					return 1
				}
				return -1
			}
			return a.Due.Compare(b.Due)
		})
		keepFrom = len(lines)
		lines = append(lines, fmt.Sprintf("未完成的承诺（%d）：", len(items)))
		lines = append(lines, promiseLines(items, in.Now.Location())...)
		keepTo = len(lines)
	}

	var units []health.Unit
	nRunning := 0
	for _, u := range in.Units {
		if u.Kind == health.KindSysession || sessionkey.IsPlannerNSKey(u.ID) {
			continue
		}
		switch {
		case running(u.State):
			nRunning++
		case in.Now.Sub(cmp.Or(u.LastActive, u.Since)) > recentWindow:
			continue
		}
		units = append(units, u)
	}
	slices.SortFunc(units, func(a, b health.Unit) int {
		if ra, rb := running(a.State), running(b.State); ra != rb {
			if ra {
				return -1
			}
			return 1
		}
		if c := b.LastActive.Compare(a.LastActive); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	lines = append(lines, fmt.Sprintf("会话（在跑 %d，最近 24 小时结束 %d）：", nRunning, len(units)-nRunning))
	unitsFrom := len(lines)
	times = make([]string, len(lines), len(lines)+len(units))
	for i, u := range units {
		if i == snapshotMaxUnits {
			lines = append(lines, fmt.Sprintf("- …另有 %d 个，见 index.json", len(units)-i))
			times = append(times, "")
			break
		}
		line, at := unitLine(in, u)
		lines = append(lines, line)
		times = append(times, at)
	}

	head := "<naozhi-snapshot at=\"" + in.Now.Format("2006-01-02 15:04 MST") + "\">"
	const tail = "</naozhi-snapshot>"
	budget := snapshotMaxRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail) - 2
	// The promise lines are kept, the rest share what they leave.
	for _, line := range lines[keepFrom:keepTo] {
		budget -= utf8.RuneCountInString(line) + 1
	}
	// The session lines come last, so the last line is one that may be cut.
	last := len(lines) - 1
	var b strings.Builder
	var keyLines, keyUnits []string
	b.WriteString(head + "\n")
	cut := 0
	for i, line := range lines {
		if i >= keepFrom && i < keepTo {
			b.WriteString(line + "\n")
			keyLines = append(keyLines, line)
			continue
		}
		if cut > 0 {
			cut++
			continue
		}
		bare := textutil.TruncateRunes(line, snapshotLineRunes)
		line = textutil.TruncateRunes(line+times[i], snapshotLineRunes)
		n := utf8.RuneCountInString(line) + 1
		// A line with more after it leaves room for the cut notice.
		reserve := cutNoticeRunes
		if i == last {
			reserve = 0
		}
		if n > budget-reserve {
			cut = 1
			continue
		}
		budget -= n
		b.WriteString(line + "\n")
		if i >= unitsFrom {
			keyUnits = append(keyUnits, bare)
		} else {
			keyLines = append(keyLines, bare)
		}
	}
	if cut > 0 {
		notice := fmt.Sprintf("…（截断，另有 %d 行，见 index.json）", cut)
		b.WriteString(notice + "\n")
		keyUnits = append(keyUnits, notice)
	}
	b.WriteString(tail)
	slices.Sort(keyUnits)
	return b.String(), strings.Join(append(keyLines, keyUnits...), "\n")
}

// unchangedSnapshot stands for a snapshot whose body is the one the
// conversation got at since, so an unchanged fleet costs one line a message.
// It still carries now, as the full one's head does: the Planner has no
// other clock, and a promise's due time or a scout finding's age is reckoned
// from it.
func unchangedSnapshot(since, now time.Time) string {
	return `<naozhi-snapshot unchanged since="` + since.Format("15:04") + `" at="` + now.Format("2006-01-02 15:04 MST") + `"/>`
}

// promiseLines lists every promise by its ID, the due time after its text:
// the texts are cut so the lines fit promiseRunes, each to no less than
// promiseTextRunes, and each line to snapshotLineRunes.
func promiseLines(items []Promise, loc *time.Location) []string {
	heads, tails := make([]string, len(items)), make([]string, len(items))
	fixed, full := 0, 0
	for i, p := range items {
		heads[i] = "- [" + p.ID + "] "
		if !p.Due.IsZero() {
			tails[i] = "（到期 " + p.Due.In(loc).Format("01-02 15:04") + "）"
		}
		n := utf8.RuneCountInString(heads[i]) + utf8.RuneCountInString(tails[i]) + 1
		fixed += n
		full += min(n+utf8.RuneCountInString(p.Text), snapshotLineRunes+1)
	}
	text := snapshotLineRunes
	if full > promiseRunes {
		text = max(promiseTextRunes, (promiseRunes-fixed)/len(items))
	}
	out := make([]string, len(items))
	for i, p := range items {
		room := min(text, snapshotLineRunes-utf8.RuneCountInString(heads[i])-utf8.RuneCountInString(tails[i]))
		out[i] = heads[i] + textutil.TruncateRunes(p.Text, max(room, promiseTextRunes)) + tails[i]
	}
	return out
}

// unitLine is u's line in the snapshot, and the time that ends it ("" for
// none).
func unitLine(in snapshotInput, u health.Unit) (line, at string) {
	// The Planner calls a session by its title or name; the key is for
	// looking it up.
	var label, cross string
	if in.Label != nil {
		label = in.Label(u.ID)
	}
	if in.Name != nil {
		cross = in.Name(u.ID)
	}
	name := u.ID
	switch {
	case label != "" && cross != "":
		name = label + "（名字 " + cross + "，" + u.ID + "）"
	case label != "" || cross != "":
		name = label + cross + "（" + u.ID + "）"
	}
	line = "- " + name + " [" + cmp.Or(stateNames[u.State], string(u.State))
	if u.ErrClass != "" {
		line += "·" + string(u.ErrClass)
	}
	line += "]"
	if u.Workspace != "" {
		ws := u.Workspace
		if in.Workspace != nil {
			ws = cmp.Or(in.Workspace(u.Workspace), ws)
		}
		line += " 工作区 " + ws
	}
	if !u.LastActive.IsZero() {
		at = " · 最后输出 " + u.LastActive.In(in.Now.Location()).Format("01-02 15:04")
	}
	return line, at
}
