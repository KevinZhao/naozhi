package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

type fakeSessions struct {
	mu        sync.Mutex
	st        SessionState
	recreated int
}

func (f *fakeSessions) State(string) SessionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeSessions) Ensure(context.Context, string, sessionview.AgentOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Exists, f.st.Alive = true, true
	return nil
}

func (f *fakeSessions) Recreate(context.Context, string, sessionview.AgentOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recreated++
	f.st = SessionState{Exists: true, Alive: true}
	return nil
}

func (*fakeSessions) Release(string) bool { return true }
func (*fakeSessions) Label(string) string { return "" }

// fakeTurns answers every turn at once with reply.
type fakeTurns struct {
	reply string
	sent  []string
}

func (f *fakeTurns) SubmitPlanner(_, text string, _ sessionview.AgentOpts, done func(string, error)) error {
	f.sent = append(f.sent, text)
	if done != nil {
		done(f.reply, nil)
	}
	return nil
}

func (*fakeTurns) PlannerReceipt(string, string) {}

func newTestAgent(t *testing.T, sess *fakeSessions, now *time.Time) (*Agent, datadir.Layout) {
	t.Helper()
	lay := datadir.FromRoot(t.TempDir())
	a := New(Config{
		Enabled:  true,
		Layout:   lay,
		Home:     t.TempDir(),
		Sessions: sess,
		Now:      func() time.Time { return *now },
	})
	return a, lay
}

// TestLaunchArgv renders planner:main's spawn options through the spawn's
// own argv builder: the five tools, dontAsk, no MCP servers, --name planner,
// the model and effort, the system prompt and the per-session settings.
func TestLaunchArgv(t *testing.T) {
	now := time.Now()
	a, lay := newTestAgent(t, &fakeSessions{}, &now)
	o := a.Opts()
	launch := session.LaunchSpec{Tools: o.Tools, PermissionMode: o.PermissionMode, Name: o.Name,
		StrictMCPConfig: o.StrictMCPConfig, SettingsFile: "/state/session-settings/p.json"}
	argv := (&cli.ClaudeProtocol{}).BuildArgs(session.ArgvSpawnOptions(o.Model, o.Effort, "", o.SystemPrompt, nil,
		launch, "/state/naozhi-settings.json", "/state/mcp.json", cli.PermissionModeDefault))
	got := strings.Join(argv, " ")
	for _, want := range []string{
		"--settings /state/session-settings/p.json",
		"--permission-mode dontAsk",
		"--tools Read,Grep,Glob,ListAgents,SendMessage",
		"--strict-mcp-config",
		"--name planner",
		"--model claude-opus-5-5[1m]",
		"--effort medium",
		"--append-system-prompt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv lacks %q: %s", want, got)
		}
	}
	for _, bad := range []string{"--dangerously-skip-permissions", "--mcp-config"} {
		if strings.Contains(got, bad) {
			t.Errorf("argv has %q: %s", bad, got)
		}
	}
	if o.Workspace != lay.PlannerWorkDir() || !o.Exempt || o.Backend != "claude" || o.CrossSessionInbound != "accept" {
		t.Errorf("opts = %+v", o)
	}
	for _, rule := range []string{"Read(~/.aws/**)", "Read(~/.ssh/**)", "Read(~/.naozhi/*.yaml)", "Read(~/.naozhi/**/*.yaml)"} {
		if !slices.Contains(o.PermissionsDeny, rule) {
			t.Errorf("deny lacks %s: %v", rule, o.PermissionsDeny)
		}
	}
	if !slices.Contains(o.PermissionsAllow, absRule("Read", filepath.Join(lay.PlannerRoot(), "**"))) {
		t.Errorf("allow lacks the state root: %v", o.PermissionsAllow)
	}
}

// TestShiftChange drives reconcile on a fake clock: no change before 08:30,
// one at 08:30 that stores the outgoing session's summary and hands it to
// the new one, none again the same day.
func TestShiftChange(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	for _, tc := range []struct {
		at   string
		want string
	}{
		{"2026-11-04 00:10", "2026-11-03"},
		{"2026-11-04 08:29", "2026-11-03"},
		{"2026-11-04 08:30", "2026-11-04"},
		{"2026-11-04 23:59", "2026-11-04"},
	} {
		at, _ := time.ParseInLocation("2006-01-02 15:04", tc.at, loc)
		if got := shiftDay(at); got != tc.want {
			t.Errorf("shiftDay(%s) = %s, want %s", tc.at, got, tc.want)
		}
	}

	now := time.Date(2026, 11, 3, 20, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, lay := newTestAgent(t, sess, &now)
	turns := &fakeTurns{reply: "## 进行中\n无"}
	a.turns = turns
	ctx := context.Background()

	a.reconcile(ctx) // first run: today's shift is the current conversation
	steps := []struct {
		at        time.Time
		recreated int
	}{
		{time.Date(2026, 11, 4, 8, 29, 0, 0, loc), 0},
		{time.Date(2026, 11, 4, 8, 30, 0, 0, loc), 1},
		{time.Date(2026, 11, 4, 9, 30, 0, 0, loc), 1},
	}
	for _, s := range steps {
		now = s.at
		a.reconcile(ctx)
		if sess.recreated != s.recreated {
			t.Fatalf("at %s: recreated %d, want %d", s.at.Format(time.Kitchen), sess.recreated, s.recreated)
		}
	}
	saved, err := os.ReadFile(filepath.Join(lay.PlannerHandoffRoot(), "2026-11-04.md"))
	if err != nil || string(saved) != turns.reply {
		t.Fatalf("handoff = %q, %v", saved, err)
	}
	if len(turns.sent) != 2 || turns.sent[0] != handoffPrompt || !strings.Contains(turns.sent[1], turns.reply) {
		t.Fatalf("turns = %q", turns.sent)
	}
	if st := a.Status(); st.Shift != "2026-11-04" {
		t.Errorf("shift = %s", st.Shift)
	}
}

// TestRenderSnapshot pins the snapshot's sections and its budget: unwired
// sources are left out, the Planner itself and daemons are not listed, ended
// units only while recent, and a large fleet is cut to a count.
func TestRenderSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	base := snapshotInput{
		Now: now,
		Units: []health.Unit{
			{ID: "dashboard:direct:a:general", Kind: health.KindSession, State: health.Active, LastActive: now.Add(-time.Minute)},
			{ID: "dashboard:direct:b:general", Kind: health.KindSession, State: health.Failed, ErrClass: "rate_limit", LastActive: now.Add(-time.Hour)},
			{ID: "dashboard:direct:old:general", Kind: health.KindSession, State: health.Idle, LastActive: now.Add(-48 * time.Hour)},
			{ID: "planner:main", Kind: health.KindSession, State: health.Active, LastActive: now},
			{ID: "sys:auto-titler", Kind: health.KindSysession, State: health.Idle, LastActive: now},
		},
		Label: func(id string) string {
			if id == "dashboard:direct:a:general" {
				return "修登录"
			}
			return ""
		},
	}

	got := renderSnapshot(base)
	want := "<naozhi-snapshot at=\"2026-11-04 10:00 UTC\">\n" +
		"会话（在跑 1，最近 24 小时结束 1）：\n" +
		"- 修登录（dashboard:direct:a:general） [活跃] · 最后活动 11-04 09:59\n" +
		"- dashboard:direct:b:general [失败·rate_limit] · 最后活动 11-04 09:00\n" +
		"</naozhi-snapshot>"
	if got != want {
		t.Fatalf("snapshot =\n%s\nwant\n%s", got, want)
	}

	withPromises := base
	withPromises.Promises = []Promise{}
	if got := renderSnapshot(withPromises); !strings.Contains(got, "答应你的（0）：") || strings.Contains(got, "等你") {
		t.Errorf("wired-but-empty promises, unwired waiting:\n%s", got)
	}

	big := base
	big.Waiting = nil
	big.Units = nil
	for i := range 200 {
		big.Units = append(big.Units, health.Unit{
			ID: fmt.Sprintf("dashboard:direct:%03d-%s:general", i, strings.Repeat("长", 60)), Kind: health.KindSession,
			State: health.Active, LastActive: now.Add(-time.Duration(i) * time.Second),
		})
	}
	got = renderSnapshot(big)
	if n := utf8.RuneCountInString(got); n > snapshotMaxRunes {
		t.Errorf("snapshot is %d runes, budget %d", n, snapshotMaxRunes)
	}
	if !strings.Contains(got, "截断") || !strings.HasSuffix(got, "</naozhi-snapshot>") {
		t.Errorf("large snapshot not cut cleanly:\n%s", got)
	}
}
