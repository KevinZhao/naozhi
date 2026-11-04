package scout

import (
	"context"
	"encoding/json"
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
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := parseOutput(c.reply)
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("err = %v", err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Fatalf("err = %v, want %q", err, c.err)
			case c.err == "" && c.name == "valid":
				if len(out.Findings) != 1 || out.Cursors.Email == "" || len(out.Calendar) != 1 || !out.Calendar[0].Online {
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
		fresh, err := st.markSeen(s.in, s.at)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fresh, s.fresh) {
			t.Errorf("step %d: fresh = %+v, want %+v", i, fresh, s.fresh)
		}
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
		Wake: func(b string) error { woken = append(woken, b); return nil },
		Now:  func() time.Time { return now },
	})
	ctx := context.Background()

	r := s.run(ctx, runID(now), TriggerManual, now)
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

	r = s.run(ctx, runID(now.Add(time.Minute)), TriggerManual, now.Add(time.Minute))
	if r.State != StateOK || r.New != 0 || r.Woke || len(woken) != 1 {
		t.Errorf("second run = %+v, woken %d", r, len(woken))
	}
	if !strings.Contains(prompts[1], `"email":"e1"`) {
		t.Errorf("second prompt lacks the cursors: %s", prompts[1])
	}

	reply = `{"findings":[],"cursors":{"email":"e2","slack":"s2","asana":""}}` + "\n好的"
	r = s.run(ctx, runID(now.Add(2*time.Minute)), TriggerManual, now.Add(2*time.Minute))
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
