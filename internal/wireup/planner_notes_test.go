package wireup

import (
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

// A session's notes count its SendMessage to the Planner as a report only
// on the prompt of before the report block: dispatched on the one with it
// (ReportBlock), the message asks on the way, and only the report block's
// line is a report.
func TestSessionReports(t *testing.T) {
	asked := time.UnixMilli(1_700_000_000_000)
	reported := asked.Add(time.Minute)
	peer := func() []time.Time { return []time.Time{asked} }
	rep := &sidecar.Report{State: "done", Summary: "好了", At: reported}
	for _, c := range []struct {
		name string
		m    sidecar.Meta
		ok   bool
		want []time.Time
	}{
		{"report block prompt", sidecar.Meta{ReportBlock: true, Report: rep}, true, []time.Time{reported}},
		{"report block prompt, no block yet", sidecar.Meta{ReportBlock: true}, true, nil},
		{"SendMessage prompt", sidecar.Meta{Report: rep}, true, []time.Time{asked, reported}},
		{"no sidecar", sidecar.Meta{}, false, []time.Time{asked}},
	} {
		if got := sessionReports(c.m, c.ok, peer); !slices.Equal(got, c.want) {
			t.Errorf("%s: reports = %v, want %v", c.name, got, c.want)
		}
	}
}
