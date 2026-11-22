package patrol

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/front"
)

const (
	// ownFile is the one file that is not a workspace's: the standing
	// tasks failing in a row and naozhi's own week.
	ownFile = "naozhi.md"
	// weeksKept is how many weekly lines the file keeps.
	weeksKept = 12

	routinesHead = "## 例行任务"
	weeksHead    = "## 每周"
)

// own makes the checks that are not a workspace's — the standing tasks
// that failed routineFails times in a row or more, each an open item — and
// writes naozhi.md with them and this week's line of naozhi's own run. They
// are made whatever workspaces are in scope: they call neither git nor gh.
// The weekly line is a line to read, not an item: the spec gives it no
// threshold to hit.
func (p *Patrol) own(rd *round) {
	names := map[string]string{}
	for _, d := range rd.dirs {
		names[d.ID] = d.Name
	}
	var lines []string
	if p.cfg.Routines != nil {
		var hits []hit
		for _, r := range p.cfg.Routines() {
			if r.ID == "" || r.Failures < routineFails {
				continue
			}
			text := fmt.Sprintf("例行任务「%s」连续失败 %d 次", clip(fold(cmp.Or(r.Title, r.ID)), 40), r.Failures)
			if r.AutoPaused {
				text += "，已自动暂停"
			}
			line := text
			if name := fold(names[r.Workspace]); name != "" {
				line += "（工作区：" + name + "）"
			}
			hits = append(hits, hit{Source: sourcePrefix + checkRoutine + ":" + r.ID, Text: text, Line: line, Workspace: r.Workspace})
		}
		p.reconcile(rd, "", hits, func(it front.Item) bool {
			g, _ := groupOf(it.Source)
			return it.From == front.FromPatrol && g == checkRoutine
		})
		lines = hitLines(hits)
	}
	path := filepath.Join(p.cfg.Dir, ownFile)
	data, _ := os.ReadFile(path)
	week, old := "", string(data)
	if p.cfg.Self != nil {
		monday := weekStart(rd.now)
		week = weekLine(rd.now, p.cfg.Self(monday, rd.now))
		// The week before was last counted by its last round, which left
		// out what came after it, and days if naozhi was paused or off. The
		// first round of a new week counts it whole.
		if !hasWeek(old, weekLabel(rd.now)) {
			before := monday.AddDate(0, 0, -7)
			old = withWeek(old, weekLine(before, p.cfg.Self(before, monday)))
		}
	}
	if err := osutil.WriteFileAtomic(path, renderOwn(rd.now, lines, p.cfg.Routines != nil, week, old), 0o600); err != nil {
		slog.Warn("planner patrol: file not written", "path", path, "err", err)
	}
}

// weekStart is the Monday 00:00 of now's week, in now's location.
func weekStart(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d-(int(now.Weekday())+6)%7, 0, 0, 0, 0, now.Location())
}

// weekLabel names now's week, "2026-W47" (ISO: weeks start on Monday).
func weekLabel(now time.Time) string {
	y, w := now.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// weekRE is a weekly line, by its label.
var weekRE = regexp.MustCompile(`^- (\d{4}-W\d{2})：`)

// hasWeek reports whether old has the weekly line labelled label.
func hasWeek(old, label string) bool {
	for l := range strings.SplitSeq(old, "\n") {
		if m := weekRE.FindStringSubmatch(l); m != nil && m[1] == label {
			return true
		}
	}
	return false
}

// withWeek is old with line as its first weekly line, in place of the one
// it had for that week.
func withWeek(old, line string) string {
	label := ""
	if m := weekRE.FindStringSubmatch(line); m != nil {
		label = m[1]
	}
	kept := []string{line}
	for l := range strings.SplitSeq(old, "\n") {
		if m := weekRE.FindStringSubmatch(l); m == nil || m[1] != label {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// weekLine is the line of now's week so far.
func weekLine(now time.Time, s Self) string {
	scout := fmt.Sprintf("巡检 %d 次、失败 %d 次", s.ScoutRuns, s.ScoutFailed)
	if s.ScoutRuns > 0 {
		scout += fmt.Sprintf("（%d%%）", (s.ScoutFailed*100+s.ScoutRuns/2)/s.ScoutRuns)
	}
	return fmt.Sprintf("- %s：%s · 没执行的动作块 %d 个 · 托管停下 %d 次 · 异常退出 %d 次",
		weekLabel(now), scout, s.Unrun, s.CustodyStopped, s.Crashes)
}

// renderOwn is naozhi.md: the standing tasks' lines (routines false: not
// checked here, no section), then the weekly lines, week first and the
// earlier weeks as old (the file as last written) has them, weeksKept in
// all. An empty week keeps old's lines as they are.
func renderOwn(now time.Time, lines []string, routines bool, week, old string) []byte {
	var b strings.Builder
	b.WriteString("# naozhi 自己\n\n")
	fmt.Fprintf(&b, "更新于 %s（naozhi 每小时检查一次，不用模型）\n\n", now.Format(timeLayout))
	if routines {
		section(&b, routinesHead, lines)
	}
	var weeks []string
	if week != "" {
		weeks = append(weeks, strings.TrimPrefix(week, "- "))
	}
	label := weekLabel(now)
	for l := range strings.SplitSeq(old, "\n") {
		if m := weekRE.FindStringSubmatch(l); m != nil && (week == "" || m[1] != label) && len(weeks) < weeksKept {
			weeks = append(weeks, strings.TrimPrefix(l, "- "))
		}
	}
	if len(weeks) > 0 {
		b.WriteString(weeksHead + "\n\n每周一行，本周的算到现在：巡检失败的比例、Planner 写错或执行失败的动作块、托管到上限停下的次数、naozhi 没正常结束的次数。\n\n")
		for _, w := range weeks {
			b.WriteString("- " + w + "\n")
		}
	}
	return []byte(strings.TrimRight(b.String(), "\n") + "\n")
}
