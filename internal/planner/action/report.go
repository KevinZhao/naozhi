package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ReportFence is the info string of a dispatched session's report block
// (§15.12 ③, D101): the gateway reads it from the session's reply, it is
// no action block of the Planner's.
const ReportFence = "naozhi-report"

// Report states.
const (
	ReportDone     = "done"
	ReportBlocked  = "blocked"
	ReportNeedUser = "need_user"
	ReportProgress = "progress"
)

// reportSummaryRunes caps a report's summary, "…" included: past it the
// summary is cut, not refused, since the report is worth having anyway.
const reportSummaryRunes = 120

// Report is a dispatched session's report: State one of the Report states,
// Summary its conclusion first, on one line.
type Report struct {
	State   string `json:"state"`
	Summary string `json:"summary"`
}

// errReportNotLast is a report block with more of the reply after it.
var errReportNotLast = errors.New("汇报块要写在这一轮回复的最末尾，后面不能再有文字")

// ParseReport reads the report a dispatched session ended its reply with:
// the last naozhi-report block, only whitespace after it. body is the reply
// before the block, the report's full text. ok is false with a nil err when
// the reply has no such block; err says what is wrong with one that does
// not parse (more text after it, not closed, not the JSON asked for), which
// counts as no report. Only the last block counts: a session working on
// naozhi itself quotes the format in its text.
func ParseReport(text string) (rep Report, body string, ok bool, err error) {
	lines := strings.Split(text, "\n")
	fs := fences(lines, ReportFence)
	if len(fs) == 0 {
		return Report{}, text, false, nil
	}
	f := fs[len(fs)-1]
	switch {
	case !f.closed:
		return Report{}, text, false, errUnclosed
	case strings.TrimSpace(strings.Join(lines[f.end:], "\n")) != "":
		return Report{}, text, false, errReportNotLast
	case len(f.body) > maxBlockBytes:
		return Report{}, text, false, fmt.Errorf("汇报块超过 %d KB", maxBlockBytes>>10)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.body), &obj); err != nil {
		return Report{}, text, false, fmt.Errorf("不是一个 JSON 对象（%v）", err)
	}
	if err := strict(f.body, &rep); err != nil {
		return Report{}, text, false, err
	}
	rep.State = strings.TrimSpace(rep.State)
	rep.Summary = cut(strings.Join(strings.Fields(rep.Summary), " "), reportSummaryRunes)
	switch rep.State {
	case ReportDone, ReportBlocked, ReportNeedUser, ReportProgress:
	default:
		return Report{}, text, false, fmt.Errorf("state 只能是 done、blocked、need_user 或 progress，不是 %q", rep.State)
	}
	if rep.Summary == "" {
		return Report{}, text, false, errors.New("缺少 summary")
	}
	return rep, strings.TrimSpace(strings.Join(lines[:f.open], "\n")), true, nil
}
