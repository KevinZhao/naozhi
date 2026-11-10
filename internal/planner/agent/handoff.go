package agent

import (
	"strconv"
	"strings"

	"github.com/naozhi/naozhi/internal/textutil"
)

// TaskLine is one row of the 任务 as the handoff's index lists it (layer 2,
// §15.12 ⑧): what the task cards show, so the incoming conversation knows
// what the user sees.
type TaskLine struct {
	// ID is the task: key, a watched session's key or a promise ID.
	ID string
	// State is the state as the cards say it (进行中, 等你, 已完成, …), and
	// Group "你开的" for a session the user opened, "" otherwise.
	State, Group string
	Title        string
	// Conclusion is the first sentence of the latest report, "" for none.
	Conclusion string
	// Digest is the session's digest file, "" for none.
	Digest string
}

// Index bounds: the lines listed and each line's length.
const (
	taskIndexMax   = 40
	taskIndexRunes = 240
)

// SetTaskIndex wires the 任务 the handoff lists; the board is built after the
// Planner, so it comes in later.
func (a *Agent) SetTaskIndex(list func() []TaskLine) {
	a.mu.Lock()
	a.tasks = list
	a.mu.Unlock()
}

// taskIndex renders the 任务 for the incoming conversation, "" when none are
// wired or listed.
func (a *Agent) taskIndex() string {
	a.mu.Lock()
	list := a.tasks
	a.mu.Unlock()
	if list == nil {
		return ""
	}
	return renderTaskIndex(list(), a.cfg.Now().Format("15:04"))
}

func renderTaskIndex(lines []TaskLine, at string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<naozhi-tasks at="` + at + `">` + "\n")
	for i, t := range lines {
		if i == taskIndexMax {
			b.WriteString("- …另外 " + strconv.Itoa(len(lines)-i) + " 个，见 index.json\n")
			break
		}
		parts := []string{t.State}
		if t.Group != "" {
			parts[0] += "（" + t.Group + "）"
		}
		parts = append(parts, oneLine(t.Title), "`"+t.ID+"`")
		if c := oneLine(t.Conclusion); c != "" {
			parts = append(parts, "结论「"+c+"」")
		}
		line := textutil.TruncateRunes(strings.Join(parts, " · "), taskIndexRunes)
		if t.Digest != "" {
			line += " · 摘要 " + t.Digest
		}
		b.WriteString("- " + line + "\n")
	}
	b.WriteString("</naozhi-tasks>")
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
