package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/parked"
	"github.com/naozhi/naozhi/internal/textutil"
)

// The handoff's check and the 搁置区 (§15.14 ⑤, D116): every item of a
// handoff names its source, and a "等你定" item the handoffs copied
// unchanged for days moves into planner/parked.md, leaving one line.

// decideHeading names the handoff section of the items waiting on the
// user's decision.
const decideHeading = "等你定"

// sourceRE finds an item's source: a session key (task: and the other
// namespaces, or any key in backticks), a promise ID, an #issue, a link, a
// date (11-14 included; two digits each, so a range like 2-3 天 is not
// one) or time the user said it, or an explicit 出处.
var sourceRE = regexp.MustCompile("`[a-z][a-z0-9_-]*:[^`\\s]+`" +
	`|\b(?:task|dashboard|feishu|slack|discord|weixin|project|cron|scratch|sys|planner):[A-Za-z0-9_#.:-]+` +
	`|\bpr_[a-z0-9]{6,}\b` +
	`|#\d+` +
	`|https?://\S+` +
	`|\d{4}-\d{1,2}-\d{1,2}` +
	`|\b(?:1[0-2]|0[1-9])-(?:3[01]|[12]\d|0[1-9])\b` +
	`|\d{1,2}月\d{1,2}日` +
	`|\b\d{1,2}[:：]\d{2}\b` +
	`|出处[:：]\s*\S`)

// hasSource reports whether item names where it came from.
func hasSource(item string) bool { return sourceRE.MatchString(item) }

// bulletRE opens a list item.
var bulletRE = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.、)])\s+`)

// headingRE opens a Markdown heading. A line like "#3902 …" is an item
// naming its issue, not a heading.
var headingRE = regexp.MustCompile(`^#{1,6}(?:\s|$)`)

// parkedLineRE is the line the gateway writes in "等你定" for the 搁置区.
var parkedLineRE = regexp.MustCompile(`^\s*(?:[-*•]\s*)?搁置\s*\d+\s*条`)

// handoffItem is one item of a handoff: a list item with the lines under
// it, or a paragraph.
type handoffItem struct {
	// start and end are its lines, [start, end).
	start, end int
	text       string
	// decide is an item of the "等你定" section.
	decide bool
	// filler is "无" or the gateway's 搁置 line: not an item.
	filler bool
}

// handoffItems splits a handoff's lines into items. sec is the line of the
// "等你定" heading, -1 when there is none, and secEnd the line its section
// ends before.
func handoffItems(lines []string) (items []handoffItem, sec, secEnd int) {
	sec, secEnd = -1, -1
	inDecide := false
	cur := -1
	closeItem := func() { cur = -1 }
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case headingRE.MatchString(t):
			closeItem()
			if inDecide && secEnd < 0 {
				secEnd = i
			}
			inDecide = strings.Contains(t, decideHeading) && sec < 0
			if inDecide {
				sec = i
			}
			continue
		case t == "":
			closeItem()
			continue
		case bulletRE.MatchString(l) || cur < 0 || inDecide && !indentedUnder(l, lines[items[cur].start]):
			closeItem()
			items = append(items, handoffItem{start: i, end: i + 1, decide: inDecide})
			cur = len(items) - 1
		default:
			items[cur].end = i + 1
		}
		it := &items[cur]
		it.text = strings.TrimSpace(it.text + " " + t)
	}
	if sec >= 0 && secEnd < 0 {
		secEnd = len(lines)
	}
	for i := range items {
		items[i].filler = isNone(items[i].text) || parkedLineRE.MatchString(items[i].text)
	}
	return items, sec, secEnd
}

// indentedUnder reports whether line l goes on with the list item opened
// by head. "等你定" is one item a line, bulleted or not (the handoff
// prompt), so there only an indented line under a bullet does.
func indentedUnder(l, head string) bool {
	return (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && bulletRE.MatchString(head)
}

// isNone reports whether an item is the 无 of an empty section.
func isNone(text string) bool {
	return strings.Trim(bulletRE.ReplaceAllString(text, ""), "（）()。. ") == "无"
}

// handoffCheck is a kind=handoff journal entry's data.
type handoffCheck struct {
	Handoff string `json:"handoff"`
	Items   int    `json:"items"`
	// Unsourced counts the items naming no source; Lines are the first
	// of them, cut short.
	Unsourced int      `json:"unsourced"`
	Lines     []string `json:"lines,omitempty"`
	Decide    int      `json:"decide"`
	Parked    int      `json:"parked"`
	// ParkedTotal is what the 搁置区 lists after the check.
	ParkedTotal int `json:"parked_total"`
}

// Journaled sample of the unsourced items.
const (
	unsourcedSample = 5
	unsourcedRunes  = 80
)

// checkHandoff checks a fresh handoff summary before it is stored and
// handed over: it counts the items without a source into the journal (to
// observe; none is refused), moves the "等你定" items untouched for more
// than parked.StaleDays days into the 搁置区, and leaves in their place
// one line saying how many it holds. day names the handoff.
func (a *Agent) checkHandoff(day, summary string) string {
	lines := strings.Split(summary, "\n")
	items, sec, secEnd := handoffItems(lines)
	ck := handoffCheck{Handoff: day}
	var decide []int
	for i, it := range items {
		if it.filler {
			continue
		}
		ck.Items++
		if !hasSource(it.text) {
			ck.Unsourced++
			if len(ck.Lines) < unsourcedSample {
				ck.Lines = append(ck.Lines, textutil.TruncateRunes(it.text, unsourcedRunes))
			}
		}
		if it.decide {
			decide = append(decide, i)
		}
	}
	ck.Decide = len(decide)

	if sec >= 0 {
		texts := make([]string, len(decide))
		for j, i := range decide {
			texts[j] = strings.TrimSpace(bulletRE.ReplaceAllString(items[i].text, ""))
		}
		keep, moved, total, err := a.parked.Sweep(texts)
		if err != nil {
			slog.Warn("planner: handoff 搁置区", "err", err)
		}
		if keep == nil {
			// Nothing moved: the items stay as written.
			keep = make([]bool, len(decide))
			for j := range keep {
				keep[j] = true
			}
			total = a.parkedCount()
		}
		ck.Parked, ck.ParkedTotal = moved, total
		for j, i := range decide {
			items[i].filler = !keep[j] // dropped below with the gateway's old line
		}
	} else {
		ck.ParkedTotal = a.parkedCount()
	}
	a.journalHandoff(ck)
	if ck.Parked > 0 || ck.Unsourced > 0 {
		slog.Info("planner: handoff checked", "handoff", day, "items", ck.Items, "unsourced", ck.Unsourced,
			"parked", ck.Parked, "parked_total", ck.ParkedTotal)
	}

	parkedLine := ""
	if ck.ParkedTotal > 0 {
		parkedLine = fmt.Sprintf("- 搁置 %d 条，见 parked.md", ck.ParkedTotal)
	}
	if sec < 0 {
		if parkedLine == "" {
			return summary
		}
		return strings.TrimRight(summary, "\n") + "\n\n## " + decideHeading + "\n" + parkedLine
	}
	// Dropped from the section: the items parked, the gateway's line a
	// copied handoff carried, and 无 once the gateway's line is there.
	drop := map[int]bool{}
	for _, it := range items {
		if it.decide && it.filler && (parkedLine != "" || !isNone(it.text)) {
			for l := it.start; l < it.end; l++ {
				drop[l] = true
			}
		}
	}
	if len(drop) == 0 && parkedLine == "" {
		return summary
	}
	// The gateway's line goes after the section's last line kept; a section
	// left with nothing says 无, as the prompt asks.
	at := sec + 1
	for l := sec + 1; l < secEnd; l++ {
		if !drop[l] && strings.TrimSpace(lines[l]) != "" {
			at = l + 1
		}
	}
	insert := parkedLine
	if insert == "" && at == sec+1 {
		insert = "无"
	}
	var out []string
	for l, line := range lines {
		if l == at && insert != "" {
			out = append(out, insert)
		}
		if !drop[l] {
			out = append(out, line)
		}
	}
	if at == len(lines) && insert != "" {
		out = append(out, insert)
	}
	return strings.Join(out, "\n")
}

func (a *Agent) journalHandoff(ck handoffCheck) {
	j := a.cfg.Journal
	if j == nil {
		return
	}
	data, err := json.Marshal(ck)
	if err == nil {
		err = j.Append(journal.Entry{TS: a.cfg.Now(), Kind: journal.KindHandoff, Unit: Key, Data: data})
	}
	if err != nil {
		slog.Warn("planner: journal the handoff check", "err", err)
	}
}

// Park puts an item the user put down into the 搁置区 (the park block):
// text is the item, source where it came from.
func (a *Agent) Park(text, source string) error {
	_, err := a.parked.Park(text, source)
	return err
}

// parkedCount is how many items the 搁置区 lists.
func (a *Agent) parkedCount() int {
	list, err := a.parked.List()
	if err != nil {
		slog.Warn("planner: read 搁置区", "err", err)
	}
	return len(list)
}

// Parked lists the 搁置区, for the morning brief.
func (a *Agent) Parked() []parked.Entry {
	list, err := a.parked.List()
	if err != nil {
		slog.Warn("planner: read 搁置区", "err", err)
	}
	return list
}
