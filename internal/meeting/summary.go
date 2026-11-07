package meeting

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CalendarEvent is the calendar event a meeting overlaps, when the calendar could be read (§5).
type CalendarEvent struct {
	Title     string
	Start     time.Time
	End       time.Time
	Organizer string
	Attendees []string
}

// eventAt is the calendar event under way at t, the one that started last when several are.
func eventAt(events []CalendarEvent, t time.Time) (CalendarEvent, bool) {
	var best CalendarEvent
	ok := false
	for _, e := range events {
		if e.Start.After(t.Add(5*time.Minute)) || !e.End.After(t) {
			continue
		}
		if !ok || e.Start.After(best.Start) {
			best, ok = e, true
		}
	}
	return best, ok
}

// summaryPrompt is the one-shot summary session's whole message (§4, MR7): the transcript is in
// it, so the session needs no tools.
func summaryPrompt(m Meeting, transcript string, now time.Time) string {
	var b strings.Builder
	b.WriteString("请为下面这场会议写纪要。只依据文字稿和会议信息，不要编造；听不清或不确定的地方照实写「不确定」。\n\n")
	b.WriteString("「你」是录音的用户本人（麦克风那一轨）；「说话人 A/B…」或「对方」是会议另一端的人。能从文字稿和参会人名单确定是谁时，写成人名并注明「（推断）」。\n\n")
	b.WriteString("用 Markdown 输出，依次是这些小节，某一节没有内容就写「无」：\n")
	b.WriteString("## 会议信息（时间、标题、参会人）\n## 结论\n## 决定\n## 待办（谁 · 做什么 · 截止）\n## 未决问题\n## 原话引用（每条前面带文字稿里的时间戳，如 `[12:34]`）\n\n")
	b.WriteString("最后单独附一个 ```json 代码块，列出全部待办：\n")
	b.WriteString(`{"todos":[{"owner":"你 或 人名","text":"做什么","due":"YYYY-MM-DD，没有就空字符串","mine":true}]}`)
	b.WriteString("\nmine 只在待办是用户本人（「你」）要做的时候为 true。\n\n")
	fmt.Fprintf(&b, "今天是 %s。\n\n<meeting>\n标题：%s\n时间：%s 起，时长 %s\n软件：%s\n",
		now.Local().Format("2006-01-02 Monday"), m.Title, m.Started.Local().Format("2006-01-02 15:04"), Clock(m.Seconds), m.App)
	if len(m.Attendees) > 0 {
		fmt.Fprintf(&b, "参会人（日历）：%s\n", strings.Join(m.Attendees, "、"))
	}
	b.WriteString("</meeting>\n\n<transcript>\n")
	b.WriteString(transcript)
	b.WriteString("\n</transcript>\n")
	return b.String()
}

// transcriptText is the lines as the summary session reads them.
func transcriptText(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "[%s] %s：%s\n", Clock(l.Start), l.Speaker, l.Text)
	}
	return b.String()
}

var jsonBlockRE = regexp.MustCompile("(?s)```json\\s*\\n(.*?)\\n?```")

// parseSummary splits the reply into the summary markdown and its to-dos; a reply without a
// readable block keeps all its text as the summary and has no to-dos.
func parseSummary(reply string) (string, []Todo) {
	locs := jsonBlockRE.FindAllStringSubmatchIndex(reply, -1)
	if len(locs) == 0 {
		return strings.TrimSpace(reply), nil
	}
	last := locs[len(locs)-1]
	var v struct {
		Todos []Todo `json:"todos"`
	}
	if json.Unmarshal([]byte(reply[last[2]:last[3]]), &v) != nil {
		return strings.TrimSpace(reply), nil
	}
	md := strings.TrimSpace(reply[:last[0]] + reply[last[1]:])
	var todos []Todo
	for _, t := range v.Todos {
		t.Text, t.Owner, t.Due = strings.TrimSpace(t.Text), strings.TrimSpace(t.Owner), strings.TrimSpace(t.Due)
		if t.Text == "" {
			continue
		}
		if t.Owner == SpeakerMe {
			t.Mine = true
		}
		todos = append(todos, t)
	}
	return md, todos
}

// dueOf is a to-do's due date as a reminder time: 09:00 local that day; zero when none.
func dueOf(s string, now time.Time) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, now.Location())
	if err != nil {
		return time.Time{}
	}
	d = d.Add(9 * time.Hour)
	if d.Before(now) {
		return time.Time{}
	}
	return d
}
