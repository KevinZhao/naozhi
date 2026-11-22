package wireup

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
)

// A side note goes on a session this node's router holds: the user's, a
// dispatched task's, a cron job's. The Planner's own, a one-shot run's, a
// session the router does not know (put away, or a remote node's) and a
// key that is none take no note.
func TestNoteTarget(t *testing.T) {
	RegisterCLIBackends()
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	for _, key := range []string{"dashboard:direct:u:general", "task:ab12", "cron:job1", "planner:main", "scout:r1", "reader:r1"} {
		r.InjectSession(key, session.NewTestProcess())
	}
	target := NoteTarget(r)
	for _, c := range []struct{ key, want string }{
		{"dashboard:direct:u:general", ""},
		{"task:ab12", ""},
		{"cron:job1", ""},
		{"planner:main", "不能写旁注"},
		{"scout:r1", "不能写旁注"},
		{"reader:r1", "不能写旁注"},
		{"dashboard:direct:gone:general", "找不到会话 dashboard:direct:gone:general"},
		{"a\nb", "不是合法的会话 key"},
		{"", "不是合法的会话 key"},
	} {
		err := target(c.key)
		if (err == nil) != (c.want == "") || err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("NoteTarget(%q) = %v, want %q", c.key, err, c.want)
		}
	}
	if err := NoteTarget(nil)("task:ab12"); err == nil {
		t.Error("NoteTarget without a router allowed a key")
	}
}

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
