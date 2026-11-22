package bgtask

import (
	"fmt"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// progressShown is how many workflows, and how many other tasks, Progress
// names; progressRunes cuts each name.
const (
	progressShown = 2
	progressRunes = 40
)

// Progress says in one line how far s's work got at now (docs/rfc/planner.md
// §15.22 ④): how long it has run, each workflow's agents done of how many
// and the phase it is in, and the other tasks by their own descriptions. The
// gateway's progress note and the task card both say it, so they cannot
// disagree. Only what is known is said: the duration is from the earliest
// start, so one start not known (a workflow's the board has not learned, a
// task only a replay started) leaves it out, where the earliest of the
// others would say less than a note before a restart did; a workflow whose
// board has no counts or phase names none. "" when none is open. The names
// are the session's own text, data the page escapes.
func (s State) Progress(now time.Time) string {
	var start time.Time
	known := true
	var workflows, others []string
	nWorkflows, nOthers := 0, 0
	for _, t := range s.Tasks {
		switch {
		case t.Start.IsZero() || t.Replayed:
			known = false
		case start.IsZero() || t.Start.Before(start):
			start = t.Start
		}
		if t.Kind == KindWorkflow {
			if nWorkflows++; len(workflows) < progressShown {
				workflows = append(workflows, workflowProgress(t))
			}
			continue
		}
		if nOthers++; len(others) < progressShown {
			others = append(others, clip(t.Summary))
		}
	}
	var parts []string
	switch d := now.Sub(start); {
	case !known || start.IsZero() || d < 0:
	case d < time.Minute:
		parts = append(parts, "刚开始")
	default:
		parts = append(parts, "已跑 "+Span(d))
	}
	parts = append(parts, workflows...)
	if more := nWorkflows - len(workflows); more > 0 {
		parts = append(parts, fmt.Sprintf("另有 %d 个工作流", more))
	}
	if nOthers > 0 {
		p := fmt.Sprintf("%d 个后台任务：%s", nOthers, strings.Join(others, "、"))
		if nOthers > len(others) {
			p += " 等"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

// workflowProgress is workflow t's part of Progress.
func workflowProgress(t Task) string {
	var say []string
	if t.Agents > 0 {
		c := fmt.Sprintf("%d 个子代理完成 %d 个", t.Agents, t.Done)
		if t.Failed > 0 {
			c += fmt.Sprintf("、失败 %d 个", t.Failed)
		}
		say = append(say, c)
	}
	if t.Phase != "" {
		say = append(say, "阶段「"+clip(t.Phase)+"」")
	}
	out := "工作流 " + clip(t.Summary)
	if len(say) > 0 {
		out += "：" + strings.Join(say, "，")
	}
	return out
}

// Output is when s's work last wrote anything after it started: the latest
// Last past its task's Start. Zero when none did: a Bash or a Monitor
// writes nothing between its start and its end, and its start is no output.
func (s State) Output() time.Time {
	var out time.Time
	for _, t := range s.Tasks {
		if t.Last.After(t.Start) {
			out = later(out, t.Last)
		}
	}
	return out
}

// Phases is the phase each open workflow is in, by its ID; a workflow whose
// phase is not known is left out.
func (s State) Phases() map[string]string {
	out := map[string]string{}
	for _, t := range s.Tasks {
		if t.Kind == KindWorkflow && t.Phase != "" {
			out[t.ID] = t.Phase
		}
	}
	return out
}

// Span says how long d is, to the minute.
func Span(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	return textutil.FormatChineseDuration(d.Truncate(time.Minute))
}

func clip(s string) string {
	return textutil.TruncateRunes(strings.Join(strings.Fields(s), " "), progressRunes)
}
