package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

func TestDue(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, loc) } // 11-02 is a Monday
	cases := []struct {
		name      string
		now, last time.Time
		slot      time.Time // zero: not due
	}{
		{"before the first slot", at(2, 7, 59), at(1, 20, 0), time.Time{}},
		{"08:00", at(2, 8, 0), at(1, 20, 0), at(2, 8, 0)},
		{"08:00 already run", at(2, 8, 10), at(2, 8, 0), time.Time{}},
		{"08:15", at(2, 8, 15), at(2, 8, 0), at(2, 8, 15)},
		{"between slots, the last one run", at(2, 9, 30), at(2, 8, 15), time.Time{}},
		{"every two hours", at(2, 12, 1), at(2, 10, 0), at(2, 12, 0)},
		{"20:00", at(2, 20, 0), at(2, 18, 0), at(2, 20, 0)},
		{"after 20:00", at(2, 23, 0), at(2, 20, 0), time.Time{}},
		// Woken at 15:30 after sleeping through 10:00, 12:00 and 14:00: one run.
		{"woken after sleeping through slots", at(2, 15, 30), at(2, 8, 15), at(2, 14, 0)},
		// A day's missed slots are not carried into the next morning.
		{"woken next day before 08:00", at(3, 7, 0), at(2, 9, 0), time.Time{}},
		{"manual run just before a slot", at(2, 14, 0), at(2, 13, 59), at(2, 14, 0)},
		{"saturday", at(7, 10, 0), at(6, 20, 0), time.Time{}},
		{"sunday", at(8, 10, 0), at(6, 20, 0), time.Time{}},
		{"first run ever", at(2, 10, 30), time.Time{}, at(2, 10, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slot, ok := due(c.now, c.last)
			if ok != !c.slot.IsZero() || !slot.Equal(c.slot) {
				t.Errorf("due(%s, %s) = %s, %v; want %s", c.now.Format(time.Kitchen), c.last.Format(time.Kitchen), slot, ok, c.slot)
			}
		})
	}
}

func TestParseOutput(t *testing.T) {
	const good = `{"findings":[{"source":"email","kind":"reply_needed","id":"m1","title":"iso 预算确认","from":"a","at":"2026-11-05T09:00:00+08:00","due":"","link":"","why":"对方在等"}],` +
		`"cursors":{"email":"2026-11-05T09:00:00+08:00","slack":"","asana":""},` +
		`"calendar":[{"id":"e1","title":"iso 周会","start":"2026-11-05T14:00:00+08:00","end":"2026-11-05T15:00:00+08:00","organizer":"b","attendees":["c"],"online":true,"link":""}]}`
	cases := []struct {
		name  string
		reply string
		err   string // "" parses
	}{
		{"valid", good, ""},
		{"in a json fence", "```json\n" + good + "\n```", ""},
		{"nothing found", `{"findings":[],"cursors":{"email":"","slack":"","asana":""},"calendar":[]}`, ""},
		{"bad json", `{"findings":[`, "unexpected EOF"},
		{"prose", "今天没有发现。", "no JSON object"},
		{"prose after", good + "\n以上。", "after the JSON"},
		{"unknown top-level field", strings.Replace(good, `"cursors"`, `"note":"x","cursors"`, 1), "unknown field"},
		{"unknown finding field", strings.Replace(good, `"why":"对方在等"`, `"why":"对方在等","score":3`, 1), "unknown field"},
		{"unknown cursor", strings.Replace(good, `"asana":""`, `"asana":"","jira":""`, 1), "unknown field"},
		{"unknown kind", strings.Replace(good, "reply_needed", "urgent", 1), "unknown kind"},
		{"unknown source", strings.Replace(good, `"source":"email"`, `"source":"teams"`, 1), "unknown source"},
		{"no id", strings.Replace(good, `"id":"m1"`, `"id":""`, 1), "no id or title"},
		{"event without a zone", strings.Replace(good, "14:00:00+08:00", "14:00:00", 1), "cannot parse"},
	}
	// A conflict is known by its start, however its meetings' IDs were
	// copied; with no start, by its pair in any order.
	conflict := strings.Replace(good, "reply_needed", "conflict", 1)
	for _, c := range []struct{ id, at, want string }{
		{"e2+e1", "", "e1+e2"},
		{"e1 + e2", "", "e1+e2"},
		{"e1+e2", "2026-11-05T15:00:00+08:00", "at:2026-11-05T07:00:00Z"},
		// The same meeting under another ID, a '+' in one: still one.
		{"e1x+e2", "2026-11-05T07:00:00Z", "at:2026-11-05T07:00:00Z"},
		{"+e1+e2", "2026-11-05T15:00:00+08:00", "at:2026-11-05T07:00:00Z"},
	} {
		in := strings.Replace(conflict, `"id":"m1"`, `"id":"`+c.id+`"`, 1)
		in = strings.Replace(in, `"at":"2026-11-05T09:00:00+08:00"`, `"at":"`+c.at+`"`, 1)
		out, err := parseOutput(in)
		if err != nil || out.Findings[0].ID != c.want {
			t.Errorf("conflict %q at %q: id %+v, err %v; want %s", c.id, c.at, out.Findings, err, c.want)
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := parseOutput(c.reply)
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("err = %v", err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Fatalf("err = %v, want %q", err, c.err)
			case c.err == "" && c.name == "valid":
				if len(out.Findings) != 1 || out.Cursors.Email == "" || len(out.events()) != 1 || !out.events()[0].Online {
					t.Errorf("out = %+v", out)
				}
			}
		})
	}
}

func TestMarkSeen(t *testing.T) {
	st := store{dir: t.TempDir()}
	day0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	a := Finding{Source: SourceEmail, Kind: KindReplyNeeded, ID: "m1"}
	b := Finding{Source: SourceSlack, Kind: KindReplyNeeded, ID: "C1/1.2"}
	steps := []struct {
		at    time.Time
		in    []Finding
		fresh []Finding
	}{
		{day0, []Finding{a, a}, []Finding{a}},
		// The same (source, ID, kind) is not new again; another kind is.
		{day0.Add(2 * time.Hour), []Finding{a, b, {Source: SourceEmail, Kind: KindDeadline, ID: "m1"}},
			[]Finding{b, {Source: SourceEmail, Kind: KindDeadline, ID: "m1"}}},
		{day0.Add(13 * 24 * time.Hour), []Finding{a}, nil},
		// Fourteen days on, a's entry has expired.
		{day0.Add(15 * 24 * time.Hour), []Finding{a}, []Finding{a}},
	}
	for i, s := range steps {
		fresh, seen := st.fresh(s.in, s.at)
		if err := st.saveSeen(seen); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fresh, s.fresh) {
			t.Errorf("step %d: fresh = %+v, want %+v", i, fresh, s.fresh)
		}
	}
}

// The brief reads the runs that went through and ended since its last
// one, oldest first.
func TestRunsSince(t *testing.T) {
	st := store{dir: t.TempDir()}
	since := time.Date(2026, 11, 2, 8, 30, 0, 0, time.Local)
	for _, r := range []Run{
		{ID: "20261102-080000-a", State: StateOK, Ended: since.Add(-time.Minute)},
		{ID: "20261102-100000-b", State: StateOK, Ended: since.Add(2 * time.Hour)},
		{ID: "20261102-120000-c", State: StateFailed, Ended: since.Add(4 * time.Hour)},
		{ID: "20261103-080000-d", State: StateOK, Ended: since.Add(24 * time.Hour)},
	} {
		if err := st.saveRun(r); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, r := range st.runsSince(since) {
		got = append(got, r.ID)
	}
	if want := []string{"20261102-100000-b", "20261103-080000-d"}; !slices.Equal(got, want) {
		t.Errorf("runsSince = %v, want %v", got, want)
	}
}

func TestSessionOpts(t *testing.T) {
	opts := sessionOpts("/w", "/s/scout-mcp.json", []string{ServerOutlook, ServerSlack, ServerAsana})
	if opts.PermissionMode != "dontAsk" || !opts.StrictMCPConfig || opts.MCPConfig != "/s/scout-mcp.json" || opts.Model != Model {
		t.Errorf("opts = %+v", opts)
	}
	if !slices.Equal(opts.Tools, opts.PermissionsAllow) {
		t.Errorf("tools %q, allow %q: want the same read list", opts.Tools, opts.PermissionsAllow)
	}
	for _, want := range []string{"mcp__aws-outlook-mcp__email_inbox", "mcp__aws-outlook-mcp__calendar_view", "mcp__slack-mcp__get_unreads",
		"mcp__slack-mcp__get_thread", "mcp__enterprise-asana-mcp__asana___GetUserTasks", "mcp__enterprise-asana-mcp__asana___SearchTasksInWorkspace"} {
		if !slices.Contains(opts.Tools, want) {
			t.Errorf("tools lack %s", want)
		}
	}
	for _, tool := range opts.Tools {
		if !strings.HasPrefix(tool, "mcp__") || slices.Contains(opts.PermissionsDeny, tool) {
			t.Errorf("tool %s: not an MCP tool, or denied", tool)
		}
	}
	for _, want := range []string{"mcp__slack-mcp__post_message", "mcp__slack-mcp__set_last_read", "mcp__aws-outlook-mcp__enable_write_tools", "mcp__aws-outlook-mcp__email_send",
		"mcp__enterprise-asana-mcp__asana___UpdateTask", "mcp__enterprise-asana-mcp__asana___CreateTask", "mcp__enterprise-asana-mcp__asana___DeleteTask"} {
		if !slices.Contains(opts.PermissionsDeny, want) {
			t.Errorf("deny lacks %s", want)
		}
	}
	// A server not configured gets neither.
	only := sessionOpts("/w", "/m", []string{ServerSlack})
	for _, r := range append(slices.Clone(only.Tools), only.PermissionsDeny...) {
		if !strings.HasPrefix(r, "mcp__slack-mcp__") {
			t.Errorf("slack-only session has %s", r)
		}
	}
}

func TestScoutMCPConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	doc := `{"mcpServers":{"slack-mcp":{"command":"s"},"exa":{"type":"http","headers":{"x-api-key":"FIXTURE-NOT-A-REAL-KEY"}},"aws-outlook-mcp":{"command":"o"}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	data, servers, err := scoutMCPConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(servers, []string{ServerOutlook, ServerSlack}) || strings.Contains(string(data), "FIXTURE") || strings.Contains(string(data), "exa") {
		t.Errorf("servers %q, file %s", servers, data)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"exa":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scoutMCPConfig(path); err == nil {
		t.Error("a file with none of the servers: want an error")
	}
}

// A run with new findings advances the cursors, caches the calendar and
// wakes the Planner; the same findings again wake nothing; a reply that
// does not parse fails the run and leaves the cursors.
func TestRun(t *testing.T) {
	root := t.TempDir()
	mcp := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"slack-mcp":{"command":"s"},"aws-outlook-mcp":{"command":"o"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	start := now.Add(4 * time.Hour).Format(time.RFC3339)
	end := now.Add(5 * time.Hour).Format(time.RFC3339)
	reply := `{"findings":[{"source":"slack","kind":"reply_needed","id":"D1/1.1","title":"iso 问进度","from":"","at":"","due":"","link":"","why":"等你回"}],` +
		`"cursors":{"email":"e1","slack":"s1","asana":""},` +
		`"calendar":[{"id":"x","title":"iso 会","start":"` + start + `","end":"` + end + `","organizer":"","attendees":[],"online":false,"link":""}]}`
	var keys, prompts, woken []string
	var gotOpts sessionview.AgentOpts
	s := New(Config{
		Layout:    datadir.FromRoot(root),
		MCPConfig: mcp,
		Session: func(_ context.Context, key string, opts sessionview.AgentOpts, text string) (string, []string, error) {
			keys, prompts, gotOpts = append(keys, key), append(prompts, text), opts
			return reply, []string{"mcp__slack-mcp__get_unreads"}, nil
		},
		Wake: func(_ func(int, error), b ...string) error { woken = append(woken, b...); return nil },
		Now:  func() time.Time { return now },
	})
	ctx := context.Background()

	r, _ := s.run(ctx, runID(now), TriggerManual, now)
	if r.State != StateOK || r.New != 1 || !r.Woke || r.Events != 1 || len(woken) != 1 {
		t.Fatalf("first run = %+v, woken %d", r, len(woken))
	}
	if !sessionkey.IsScoutKey(keys[0]) || gotOpts.MCPConfig != datadir.FromRoot(root).ScoutMCPConfigPath() {
		t.Errorf("key %s, opts %+v", keys[0], gotOpts)
	}
	if !strings.HasPrefix(woken[0], `<naozhi-scout findings="1"`) || !strings.Contains(woken[0], "D1/1.1") {
		t.Errorf("block = %s", woken[0])
	}
	if c := s.st.cursors(); c.Email != "e1" || c.Slack != "s1" {
		t.Errorf("cursors = %+v", c)
	}
	if cal := s.Calendar(); len(cal) != 1 || cal[0].ID != "x" {
		t.Errorf("calendar = %+v", cal)
	}

	r, _ = s.run(ctx, runID(now.Add(time.Minute)), TriggerManual, now.Add(time.Minute))
	if r.State != StateOK || r.New != 0 || r.Woke || len(woken) != 1 {
		t.Errorf("second run = %+v, woken %d", r, len(woken))
	}
	if !strings.Contains(prompts[1], `"email":"e1"`) {
		t.Errorf("second prompt lacks the cursors: %s", prompts[1])
	}

	reply = `{"findings":[],"cursors":{"email":"e2","slack":"s2","asana":""}}` + "\n好的"
	r, _ = s.run(ctx, runID(now.Add(2*time.Minute)), TriggerManual, now.Add(2*time.Minute))
	if r.State != StateFailed || !strings.Contains(r.Error, "after the JSON") {
		t.Errorf("bad run = %+v", r)
	}
	if c := s.st.cursors(); c.Email != "e1" {
		t.Errorf("cursors after a failed run = %+v, want unchanged", c)
	}
	last, ok := s.st.lastRun()
	if !ok || last.State != StateFailed {
		t.Errorf("last run record = %+v", last)
	}
	var f map[string]any
	data, _ := os.ReadFile(datadir.FromRoot(root).ScoutMCPConfigPath())
	if json.Unmarshal(data, &f) != nil || len(f["mcpServers"].(map[string]any)) != 2 {
		t.Errorf("scout mcp config = %s", data)
	}
}

// A run's findings count as seen, and its cursors move, only once the
// Planner took them or will never take them (ErrPlannerOff); a wake that
// failed otherwise leaves both, so the next run wakes with them again. A
// reply without the calendar key keeps the cached calendar, less what is
// over. A run shutdown cut short leaves no record.
func TestRunWakeAndCalendar(t *testing.T) {
	root := t.TempDir()
	mcp := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"aws-outlook-mcp":{"command":"o"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	event := func(id string, start, end time.Duration) string {
		return `{"id":"` + id + `","title":"iso 会","start":"` + now.Add(start).Format(time.RFC3339) + `","end":"` +
			now.Add(end).Format(time.RFC3339) + `","organizer":"","attendees":[],"online":false,"link":""}`
	}
	const finding = `{"source":"email","kind":"reply_needed","id":"m1","title":"iso 预算","from":"","at":"","due":"","link":"","why":"等你回"}`
	var wakeErr, sessErr error
	var reply string
	woken := 0
	s := New(Config{
		Layout:    datadir.FromRoot(root),
		MCPConfig: mcp,
		Session: func(context.Context, string, sessionview.AgentOpts, string) (string, []string, error) {
			return reply, nil, sessErr
		},
		Wake: func(func(int, error), ...string) error { woken++; return wakeErr },
		Now:  func() time.Time { return now },
	})
	steps := []struct {
		name     string
		calendar string // "" leaves the key out
		wakeErr  error
		woken    int
		email    string // the cursor after the run
		cached   []string
	}{
		{name: "Planner busy", calendar: `[` + event("a", time.Hour, 2*time.Hour) + `,` + event("b", 3*time.Hour, 4*time.Hour) + `]`,
			wakeErr: errors.New("服务忙"), woken: 1, cached: []string{"a", "b"}},
		{name: "woken on the next run", calendar: "", woken: 2, email: "e1", cached: []string{"a", "b"}},
		{name: "seen", calendar: "[]", woken: 2, email: "e1"},
	}
	for _, st := range steps {
		reply = `{"findings":[` + finding + `],"cursors":{"email":"e1","slack":"","asana":""}`
		if st.calendar != "" {
			reply += `,"calendar":` + st.calendar
		}
		reply += "}"
		wakeErr = st.wakeErr
		r, kept := s.run(context.Background(), runID(now), TriggerManual, now)
		if r.State != StateOK || !kept || woken != st.woken {
			t.Errorf("%s: run %+v kept %v, woken %d; want %d", st.name, r, kept, woken, st.woken)
		}
		if c := s.st.cursors(); c.Email != st.email {
			t.Errorf("%s: email cursor %q, want %q", st.name, c.Email, st.email)
		}
		var ids []string
		for _, e := range s.Calendar() {
			ids = append(ids, e.ID)
		}
		if !slices.Equal(ids, st.cached) {
			t.Errorf("%s: calendar %q, want %q", st.name, ids, st.calendar)
		}
		now = now.Add(90 * time.Minute)
	}

	// The Planner off: kept on disk, as seen.
	reply = `{"findings":[` + strings.Replace(finding, `"m1"`, `"m2"`, 1) + `],"cursors":{"email":"e2","slack":"","asana":""}}`
	wakeErr = fmt.Errorf("%w: off", ErrPlannerOff)
	if r, _ := s.run(context.Background(), runID(now), TriggerManual, now); r.State != StateOK || r.Woke || s.st.cursors().Email != "e2" {
		t.Errorf("Planner off: run %+v, cursors %+v", r, s.st.cursors())
	}
	if fresh, _ := s.st.fresh([]Finding{{Source: SourceEmail, Kind: KindReplyNeeded, ID: "m2"}}, now); len(fresh) != 0 {
		t.Errorf("Planner off: m2 not marked seen")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sessErr = context.Canceled
	before, _ := s.st.lastRun()
	if _, kept := s.run(ctx, runID(now.Add(time.Hour)), TriggerSchedule, now.Add(time.Hour)); kept {
		t.Error("a run shutdown cut short was kept")
	}
	if after, _ := s.st.lastRun(); after.ID != before.ID {
		t.Errorf("a run shutdown cut short left a record: %+v", after)
	}
}

// TestConflicts: the gateway's overlaps — a pair once, its ID sorted;
// touching, all-day and zero-length events conflict with nothing.
func TestConflicts(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.Local)
	ev := func(id string, start, end time.Duration) Event {
		return Event{ID: id, Title: "iso " + id, Start: at.Add(start), End: at.Add(end)}
	}
	h := time.Hour
	key := func(d time.Duration) string { return "at:" + at.Add(d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name   string
		events []Event
		want   []string
	}{
		{"none", []Event{ev("a", 0, h), ev("b", 2*h, 3*h)}, nil},
		{"touching", []Event{ev("a", 0, h), ev("b", h, 2*h)}, nil},
		{"overlap, listed out of order", []Event{ev("z", 30*time.Minute, 2*h), ev("a", 0, h)}, []string{key(30 * time.Minute)}},
		// a+c and b+c both start at 1:30: one alert.
		{"three-way", []Event{ev("a", 0, 3*h), ev("b", h, 2*h), ev("c", 90*time.Minute, 4*h)}, []string{key(h), key(90 * time.Minute)}},
		{"all-day and zero-length", []Event{ev("day", 0, 24*h), ev("dot", h, h), ev("a", 30*time.Minute, 2*h)}, nil},
	} {
		var got []string
		for _, f := range conflicts(tc.events) {
			if f.Kind != KindConflict || f.Source != SourceCalendar || f.Title == "" {
				t.Errorf("%s: finding %+v", tc.name, f)
			}
			got = append(got, f.ID)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: conflicts %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRunConflictsAndMarkAs: the model's own conflict findings are dropped
// for the gateway's; a session that read mail with markAs fails the run and
// leaves the cursors.
func TestRunConflictsAndMarkAs(t *testing.T) {
	root := t.TempDir()
	mcp := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"aws-outlook-mcp":{"command":"o"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	ts := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	reply := `{"findings":[{"source":"calendar","kind":"conflict","id":"q+r","title":"iso 模型说冲突","from":"","at":"","due":"","link":"","why":""}],` +
		`"cursors":{"email":"e1","slack":"","asana":""},"calendar":[` +
		`{"id":"b","title":"iso 乙","start":"` + ts(90*time.Minute) + `","end":"` + ts(3*time.Hour) + `","organizer":"","attendees":[],"online":false,"link":""},` +
		`{"id":"a","title":"iso 甲","start":"` + ts(time.Hour) + `","end":"` + ts(2*time.Hour) + `","organizer":"","attendees":[],"online":false,"link":""}]}`
	tools := []string{"mcp__aws-outlook-mcp__calendar_view"}
	var woken []string
	s := New(Config{
		Layout:    datadir.FromRoot(root),
		MCPConfig: mcp,
		Session: func(context.Context, string, sessionview.AgentOpts, string) (string, []string, error) {
			return reply, tools, nil
		},
		Wake: func(_ func(int, error), b ...string) error { woken = append(woken, b...); return nil },
		Now:  func() time.Time { return now },
	})
	want := "at:" + now.Add(90*time.Minute).UTC().Format(time.RFC3339)
	r, _ := s.run(context.Background(), runID(now), TriggerManual, now)
	if r.State != StateOK || r.New != 1 || len(r.Findings) != 1 || r.Findings[0].ID != want {
		t.Fatalf("run = %+v", r)
	}
	if len(woken) != 1 || strings.Contains(woken[0], "q+r") || !strings.Contains(woken[0], want) {
		t.Errorf("woken with %q", woken)
	}

	// The model copies the meeting IDs differently next time: same overlap,
	// nothing new, no wake.
	reply = strings.ReplaceAll(strings.ReplaceAll(reply, `"id":"a"`, `"id":"a2"`), `"id":"b"`, `"id":"b+x"`)
	r, _ = s.run(context.Background(), runID(now.Add(30*time.Second)), TriggerManual, now.Add(30*time.Second))
	if r.State != StateOK || r.New != 0 || len(woken) != 1 {
		t.Errorf("rerun with recopied IDs = %+v, woken %d", r, len(woken))
	}

	tools = append(tools, "mcp__aws-outlook-mcp__email_read"+MarkAsSuffix)
	r, _ = s.run(context.Background(), runID(now.Add(time.Minute)), TriggerManual, now.Add(time.Minute))
	if r.State != StateFailed || !strings.Contains(r.Error, "markAs") {
		t.Errorf("markAs run = %+v", r)
	}
}

// More new findings than one reply's action blocks can answer go to the
// Planner in batches, one turn each, and every finding is in exactly one.
func TestRunBatches(t *testing.T) {
	root := t.TempDir()
	mcp := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"slack-mcp":{"command":"s"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
	var items []string
	for i := range 11 {
		items = append(items, fmt.Sprintf(`{"source":"slack","kind":"fyi","id":"iso-%02d","title":"iso 消息 %d","from":"","at":"","due":"","link":"","why":""}`, i, i))
	}
	reply := `{"findings":[` + strings.Join(items, ",") + `],"cursors":{"email":"","slack":"s1","asana":""}}`
	var calls [][]string
	s := New(Config{
		Layout:    datadir.FromRoot(root),
		MCPConfig: mcp,
		Session: func(context.Context, string, sessionview.AgentOpts, string) (string, []string, error) {
			return reply, nil, nil
		},
		Wake: func(_ func(int, error), b ...string) error { calls = append(calls, b); return nil },
		Now:  func() time.Time { return now },
	})
	r, _ := s.run(context.Background(), runID(now), TriggerManual, now)
	if r.State != StateOK || r.New != 11 || !r.Woke || len(calls) != 1 {
		t.Fatalf("run = %+v, wakes %d", r, len(calls))
	}
	b := calls[0]
	if len(b) != 2 || !strings.HasPrefix(b[0], `<naozhi-scout findings="10"`) || !strings.HasPrefix(b[1], `<naozhi-scout findings="1"`) ||
		!strings.Contains(b[0], `batch="1/2"`) || !strings.Contains(b[1], `batch="2/2"`) {
		t.Fatalf("blocks = %q", b)
	}
	for i := range 11 {
		id := fmt.Sprintf(`"id":"iso-%02d"`, i)
		if n := strings.Count(b[0], id) + strings.Count(b[1], id); n != 1 {
			t.Errorf("%s is in %d blocks", id, n)
		}
	}
}

// TestRunLaterBatchKept: a later batch stays pending on disk until it is
// submitted, so one a restart cut off or the Planner refused goes to the
// next run's wake, even with the cursors moved on; one still queued in this
// process, submitted, or never to be taken does not.
func TestRunLaterBatchKept(t *testing.T) {
	busy := errors.New("服务忙")
	for _, tc := range []struct {
		name    string
		later   error // what sent is told of batch 2; nil with told false is nothing yet
		told    bool
		restart bool
		again   bool // batch 2's finding in the next run's wake
	}{
		{name: "restart before batch 2", restart: true, again: true},
		{name: "batch 2 refused", later: busy, told: true, again: true},
		{name: "batch 2 still queued"},
		{name: "batch 2 submitted", told: true},
		{name: "Planner stopped", later: fmt.Errorf("%w: stopped", ErrPlannerOff), told: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mcp := filepath.Join(root, "mcp.json")
			if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"slack-mcp":{"command":"s"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local)
			var items []string
			for i := range 11 {
				items = append(items, fmt.Sprintf(`{"source":"slack","kind":"fyi","id":"iso-%02d","title":"iso 消息 %d","from":"","at":"","due":"","link":"","why":""}`, i, i))
			}
			reply := `{"findings":[` + strings.Join(items, ",") + `],"cursors":{"email":"","slack":"s1","asana":""}}`
			var calls [][]string
			var sent func(int, error)
			cfg := Config{
				Layout:    datadir.FromRoot(root),
				MCPConfig: mcp,
				Session: func(context.Context, string, sessionview.AgentOpts, string) (string, []string, error) {
					return reply, nil, nil
				},
				Wake: func(s func(int, error), b ...string) error { calls, sent = append(calls, b), s; return nil },
				Now:  func() time.Time { return now },
			}
			s := New(cfg)
			if r, _ := s.run(context.Background(), runID(now), TriggerManual, now); !r.Woke || len(calls) != 1 || len(calls[0]) != 2 {
				t.Fatalf("first run = %+v, wakes %q", r, calls)
			}
			if tc.told {
				sent(1, tc.later)
			}
			if tc.restart {
				s = New(cfg)
			}
			now = now.Add(2 * time.Hour)
			reply = `{"findings":[],"cursors":{"email":"","slack":"s2","asana":""}}`
			if r, _ := s.run(context.Background(), runID(now), TriggerManual, now); r.State != StateOK {
				t.Fatalf("next run = %+v", r)
			}
			next := fmt.Sprint(calls[1:])
			if got := strings.Contains(next, `"id":"iso-10"`); got != tc.again || strings.Contains(next, `"id":"iso-00"`) {
				t.Errorf("next run's wake = %q; want batch 2 again: %v", next, tc.again)
			}
		})
	}
}
