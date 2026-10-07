package subagent

import "strings"

// harnessMark opens the first prompt of every workflow agent; the task the
// script computed follows harnessEnd, each of its lines indented two spaces.
const (
	harnessMark = "[Workflow harness — computed task]"
	harnessEnd  = "follows:\n"
)

// StripHarnessFraming returns the task inside a workflow agent's framed
// prompt: what follows the frame, with up to two leading spaces taken off
// each line. Text without the frame comes back as it is, ok false.
func StripHarnessFraming(text string) (task string, ok bool) {
	if !strings.HasPrefix(text, harnessMark) {
		return text, false
	}
	i := strings.Index(text, harnessEnd)
	if i < 0 {
		return text, false
	}
	lines := strings.Split(text[i+len(harnessEnd):], "\n")
	for j, l := range lines {
		l, _ = strings.CutPrefix(l, " ")
		lines[j], _ = strings.CutPrefix(l, " ")
	}
	return strings.Join(lines, "\n"), true
}
