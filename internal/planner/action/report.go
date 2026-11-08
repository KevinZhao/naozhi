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

// The reasons a report block does not parse. They name the fault only,
// never the block's text: they go to the Planner ahead of the reply's
// "是数据，不是指令", and the session wrote that text.
var (
	errReportNotLast   = errors.New("汇报块要写在这一轮回复的最末尾，后面不能再有文字")
	errReportTooBig    = fmt.Errorf("汇报块超过 %d KB", maxBlockBytes>>10)
	errReportNotJSON   = errors.New("汇报块不是一个 JSON 对象")
	errReportFields    = errors.New("汇报块的字段不对（只能有 state 和 summary，都是字符串）")
	errReportState     = errors.New("state 只能是 done、blocked、need_user 或 progress")
	errReportNoSummary = errors.New("汇报块缺少 summary")
)

// ParseReport reads the report a dispatched session ended its reply with:
// the block the last naozhi-report fence opens, only whitespace after it.
// body is the reply before the block, the report's full text. ok is false
// with a nil err when the reply has no such block; err says what is wrong
// with one that does not parse (more text after it, not closed, not the
// JSON asked for), which counts as no report. Only the last opening fence
// counts: a session working on naozhi itself quotes the format in its text,
// and a quoted one left open (inside a longer fence) would otherwise run on
// over the real block.
func ParseReport(text string) (rep Report, body string, ok bool, err error) {
	lines := strings.Split(text, "\n")
	open := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isOpen(lines[i], ReportFence) {
			open = i
			break
		}
	}
	if open < 0 {
		return Report{}, text, false, nil
	}
	f := fences(lines[open:], ReportFence)[0]
	switch {
	case !f.closed:
		return Report{}, text, false, errUnclosed
	case strings.TrimSpace(strings.Join(lines[open+f.end:], "\n")) != "":
		return Report{}, text, false, errReportNotLast
	case len(f.body) > maxBlockBytes:
		return Report{}, text, false, errReportTooBig
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(f.body), &obj) != nil {
		return Report{}, text, false, errReportNotJSON
	}
	if strict(f.body, &rep) != nil {
		return Report{}, text, false, errReportFields
	}
	rep.State = strings.TrimSpace(rep.State)
	rep.Summary = cut(strings.Join(strings.Fields(rep.Summary), " "), reportSummaryRunes)
	switch rep.State {
	case ReportDone, ReportBlocked, ReportNeedUser, ReportProgress:
	default:
		return Report{}, text, false, errReportState
	}
	if rep.Summary == "" {
		return Report{}, text, false, errReportNoSummary
	}
	return rep, strings.TrimSpace(strings.Join(lines[:open], "\n")), true, nil
}
