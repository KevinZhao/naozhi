package scout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

func TestBriefDue(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.Local) } // 11-02 is a Monday
	meeting := func(id string, start time.Time, people ...string) Event {
		return Event{ID: id, Title: "iso 会", Start: start, End: start.Add(time.Hour), Organizer: "me@x", Attendees: people}
	}
	ten := meeting("m1", at(2, 10, 0), "bob@x")
	cases := []struct {
		name   string
		now    time.Time
		events []Event
		done   []briefed
		want   string // "" for none due
	}{
		{"15 minutes before", at(2, 9, 45), []Event{ten}, nil, "m1"},
		{"16 minutes before", at(2, 9, 44), []Event{ten}, nil, ""},
		{"a minute before", at(2, 9, 59), []Event{ten}, nil, "m1"},
		// Asleep through 09:45: woken after the start, the meeting is skipped.
		{"woken after the start", at(2, 10, 0), []Event{ten}, nil, ""},
		{"no one else", at(2, 9, 50), []Event{meeting("m2", at(2, 10, 0)), meeting("m3", at(2, 10, 0), " ME@x")}, nil, ""},
		{"invited by someone", at(2, 9, 50), []Event{{ID: "m4", Start: at(2, 10, 0), End: at(2, 11, 0), Organizer: "bob@x", Attendees: []string{"me@x"}}}, nil, "m4"},
		// The calendar tool lists no attendees: the scout writes the user as
		// "me" on an invitation, and nothing on a meeting of their own.
		{"invited, no attendee list", at(2, 9, 50), []Event{{ID: "m7", Start: at(2, 10, 0), End: at(2, 11, 0), Organizer: "bob@x", Attendees: []string{"me"}}}, nil, "m7"},
		{"their own, no attendee list", at(2, 9, 50), []Event{{ID: "m8", Start: at(2, 10, 0), End: at(2, 11, 0), Organizer: "me"}}, nil, ""},
		{"already briefed", at(2, 9, 50), []Event{ten}, []briefed{{ID: "m1", Start: at(2, 10, 0)}}, ""},
		{"moved after its brief", at(2, 13, 50), []Event{meeting("m1", at(2, 14, 0), "bob@x")}, []briefed{{ID: "m1", Start: at(2, 10, 0)}}, "m1"},
		{"the first with others", at(2, 9, 50), []Event{meeting("m2", at(2, 10, 0)), ten}, nil, "m1"},
		{"quiet hours", at(2, 7, 50), []Event{meeting("m5", at(2, 8, 0), "bob@x")}, nil, ""},
		{"quiet hours over", at(2, 8, 0), []Event{meeting("m5", at(2, 8, 5), "bob@x")}, nil, "m5"},
		{"saturday", at(7, 9, 50), []Event{meeting("m6", at(7, 10, 0), "bob@x")}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, ok := briefDue(c.now, c.events, c.done)
			if ok != (c.want != "") || e.ID != c.want {
				t.Errorf("briefDue = %q, %v; want %q", e.ID, ok, c.want)
			}
		})
	}
}

// A brief due runs a scout session with the meeting in its prompt and
// hands its points to the Planner as a <naozhi-scout kind="meeting">
// block, once per meeting, whatever the outcome; none runs while the
// Planner is off.
func TestBriefRun(t *testing.T) {
	root := t.TempDir()
	mcp := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"slack-mcp":{"command":"s"},"aws-outlook-mcp":{"command":"o"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 9, 46, 0, 0, time.Local)
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	st := store{dir: datadir.FromRoot(root).PlannerScoutRoot()}
	cal := []Event{{ID: "m1", Title: "iso 预算评审", Start: start, End: start.Add(time.Hour), Organizer: "a@x", Attendees: []string{"b@x"}}}
	if _, err := st.saveCalendar(&cal, now); err != nil {
		t.Fatal(err)
	}
	reply := `{"why":"定预算","last":"","waiting":"等你的数字","prepare":"带上表",` +
		`"related":[{"source":"email","id":"e1","title":"iso 预算草稿","from":"a","at":"","link":"","note":"上周发的"}]}`
	var prompts, blocks []string
	on := false
	s := New(Config{
		Layout:    datadir.FromRoot(root),
		MCPConfig: mcp,
		Session: func(_ context.Context, _ string, _ sessionview.AgentOpts, text string) (string, []string, error) {
			prompts = append(prompts, text)
			return reply, nil, nil
		},
		Brief:     func(b string) error { blocks = append(blocks, b); return nil },
		PlannerOn: func() bool { return on },
		Now:       func() time.Time { return now },
	})
	s.ctx = context.Background()
	check := func() { s.checkBrief(now); s.runs.Wait() }

	check()
	if len(prompts) != 0 {
		t.Fatalf("Planner off: %d sessions, want none", len(prompts))
	}
	on = true
	check()
	check()
	if len(prompts) != 1 || len(blocks) != 1 {
		t.Fatalf("sessions %d, blocks %d; want one each", len(prompts), len(blocks))
	}
	if !strings.Contains(prompts[0], "iso 预算评审") || !strings.Contains(prompts[0], "b@x") {
		t.Errorf("prompt lacks the meeting: %s", prompts[0])
	}
	if !strings.HasPrefix(blocks[0], `<naozhi-scout kind="meeting" start="10:00"`) || !strings.Contains(blocks[0], "等你的数字") ||
		!strings.Contains(blocks[0], "notify") {
		t.Errorf("block = %s", blocks[0])
	}
	if b := st.briefed(); len(b) != 1 || b[0].ID != "m1" || b[0].State != StateOK || !b[0].Woke {
		t.Errorf("briefed = %+v", b)
	}

	// A reply that does not parse fails the brief, still only once.
	cal[0].ID = "m2"
	if _, err := st.saveCalendar(&cal, now); err != nil {
		t.Fatal(err)
	}
	reply = `{"why":"x","notes":[]}`
	check()
	check()
	b := st.briefed()
	if len(prompts) != 2 || len(blocks) != 1 || len(b) != 2 || b[1].State != StateFailed || !strings.Contains(b[1].Error, "unknown field") {
		t.Errorf("sessions %d, blocks %d, briefed %+v", len(prompts), len(blocks), b)
	}
}
