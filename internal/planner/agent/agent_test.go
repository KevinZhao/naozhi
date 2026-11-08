package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

type fakeSessions struct {
	mu        sync.Mutex
	st        SessionState
	recreated int
	// recreateErr fails Recreate after the old session is gone, as
	// Router.ResetAndRecreate does.
	recreateErr error
	// names is the --name of every Ensure and Recreate, in order.
	names []string
	// compacted is CompactedSince's answer.
	compacted bool
}

func (f *fakeSessions) CompactedSince(string, time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compacted
}

func (f *fakeSessions) State(key string) SessionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key != Key {
		return SessionState{Exists: key == refKey}
	}
	return f.st
}

func (f *fakeSessions) Ensure(_ context.Context, _ string, opts sessionview.AgentOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, opts.Name)
	f.st.Exists, f.st.Alive = true, true
	return nil
}

func (f *fakeSessions) Recreate(_ context.Context, _ string, opts sessionview.AgentOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, opts.Name)
	f.recreated++
	if f.recreateErr != nil {
		f.st = SessionState{}
		return f.recreateErr
	}
	f.st = SessionState{Exists: true, Alive: true}
	return nil
}

func (f *fakeSessions) Release(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running {
		return false
	}
	f.st.Alive = false
	return true
}
func (*fakeSessions) Label(key string) string {
	if key == refKey {
		return "修 <登录>"
	}
	return ""
}

func (*fakeSessions) Name(key string) string {
	if key == refKey {
		return "iso-login"
	}
	return ""
}

// refKey is the one session besides planner:main fakeSessions knows.
const refKey = "dashboard:direct:a:general"

// fakeTurns answers every turn at once with reply, or ends it with
// turnErr.
type fakeTurns struct {
	reply   string
	turnErr error
	sent    []string
	images  [][]clievent.Attachment
	srcs    []Source
	said    []*Words
	notes   []string
	sources []Source
	words   []Words
}

func (f *fakeTurns) SubmitPlanner(_ string, src Source, said *Words, text string, images []clievent.Attachment, _ sessionview.AgentOpts, done func(string, error)) (bool, error) {
	f.sent = append(f.sent, text)
	f.said = append(f.said, said)
	f.images = append(f.images, images)
	f.srcs = append(f.srcs, src)
	if done != nil {
		done(f.reply, f.turnErr)
	}
	return false, nil
}

func (f *fakeTurns) PlannerTurnSources(string) []Source { return f.sources }
func (f *fakeTurns) PlannerTurnWords(string) []Words    { return f.words }
func (*fakeTurns) PlannerReceipt(string, string)        {}
func (f *fakeTurns) PlannerNote(_, text string)         { f.notes = append(f.notes, text) }
func (*fakeTurns) DropPlannerQueue(string)              {}

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
// own argv builder: the five tools, dontAsk, no MCP servers, the instance's
// --name,
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
		"--name " + NameFor(lay.PlannerRoot()),
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
	if o.Workspace != lay.PlannerWorkDir() || !o.Exempt || o.Backend != "claude" || o.CrossSessionInbound != "accept" || o.PromptCacheTTL != "1h" {
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

// The Planner's --name is its instance's: two state roots on one machine
// get two names, while restarts and shift changes keep one root's name, the
// one the sessions it dispatched were told.
func TestName(t *testing.T) {
	if a, b := NameFor("/tmp/iso-a/planner"), NameFor("/tmp/iso-b/planner"); a == b {
		t.Errorf("two roots share the name %s", a)
	}
	if !regexp.MustCompile(`^planner-[0-9a-f]{6}$`).MatchString(NameFor("/x")) {
		t.Errorf("name = %s", NameFor("/x"))
	}
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 3, 20, 0, 0, 0, loc)
	sess := &fakeSessions{}
	a, lay := newTestAgent(t, sess, &now)
	a.turns = &fakeTurns{reply: "## 进行中\n无"}
	want := NameFor(lay.PlannerRoot())
	a.reconcile(context.Background()) // spawn
	sess.st.SessionID = "s1"
	now = time.Date(2026, 11, 4, 8, 30, 0, 0, loc)
	a.reconcile(context.Background()) // shift change
	if len(sess.names) != 2 || sess.names[0] != want || sess.names[1] != want || a.Name() != want {
		t.Errorf("names = %q, Name() = %s, want %s", sess.names, a.Name(), want)
	}
	if again := New(Config{Layout: lay, Sessions: sess}); again.Name() != want {
		t.Errorf("after a restart: %s, want %s", again.Name(), want)
	}
}

func TestAllowRulesWorkspaces(t *testing.T) {
	got := allowRules("/s/planner", "/s/events", []string{"/w/a", "/w/b", "/w/a"})
	for _, want := range []string{"Read(//s/planner/**)", "Read(//w/a/**)", "Read(//w/b/**)"} {
		if !slices.Contains(got, want) {
			t.Errorf("allow lacks %s: %v", want, got)
		}
	}
	if n := len(got); n != 6 {
		t.Errorf("len = %d, want 6 (duplicate workspace collapsed): %v", n, got)
	}
}

// TestStatusWorkspaces: the status names the registered workspaces by ID,
// for the page to show a dispatch's workspace by name.
func TestStatusWorkspaces(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	if st := a.Status(); st.Workspaces != nil {
		t.Fatalf("no registry: workspaces = %v", st.Workspaces)
	}
	reg := workspace.Load("", "local")
	w, ok := reg.Ensure(t.TempDir())
	if !ok {
		t.Fatal("workspace not registered")
	}
	a.cfg.Workspaces = reg
	if got := a.Status().Workspaces; got[w.ID] != w.Name || len(got) != 1 {
		t.Fatalf("workspaces = %v, want %s: %s", got, w.ID, w.Name)
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
		running   bool
		recreated int
	}{
		{time.Date(2026, 11, 4, 8, 29, 0, 0, loc), false, 0},
		{time.Date(2026, 11, 4, 8, 30, 0, 0, loc), true, 0}, // waits out a running turn
		{time.Date(2026, 11, 4, 8, 31, 0, 0, loc), false, 1},
		{time.Date(2026, 11, 4, 9, 30, 0, 0, loc), false, 1},
	}
	for _, s := range steps {
		now = s.at
		sess.mu.Lock()
		sess.st.Running = s.running
		sess.mu.Unlock()
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

// TestShiftChangeRestartFails: a failed restart leaves the Planner gone and
// the stored summary pending; revival waits out the backoff, and the fresh
// conversation it starts still gets the summary.
func TestShiftChangeRestartFails(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 3, 20, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, _ := newTestAgent(t, sess, &now)
	turns := &fakeTurns{reply: "## 进行中\n无"}
	a.turns = turns
	ctx := context.Background()
	a.reconcile(ctx)

	sess.recreateErr = errors.New("spawn failed")
	now = time.Date(2026, 11, 4, 8, 30, 0, 0, loc)
	a.reconcile(ctx)
	sess.recreateErr = nil
	if st := a.Status(); st.Alive || st.Shift != "2026-11-03" || len(turns.sent) != 1 {
		t.Fatalf("after failed restart: %+v, turns %q", st, turns.sent)
	}

	now = now.Add(30 * time.Second) // inside the backoff
	a.reconcile(ctx)
	if a.Status().Alive {
		t.Fatal("revived inside the backoff")
	}

	now = now.Add(time.Minute)
	a.reconcile(ctx)
	if st := a.Status(); !st.Alive || st.Shift != "2026-11-04" {
		t.Fatalf("after revival: %+v", st)
	}
	if len(turns.sent) != 2 || !strings.Contains(turns.sent[1], turns.reply) || sess.recreated != 1 {
		t.Fatalf("turns = %q, recreated %d", turns.sent, sess.recreated)
	}
	a.reconcile(ctx)
	if len(turns.sent) != 2 {
		t.Fatalf("handoff sent again: %q", turns.sent)
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
		Name: func(id string) string {
			return map[string]string{"dashboard:direct:a:general": "iso-login", "dashboard:direct:b:general": "iso-b"}[id]
		},
	}

	got := renderSnapshot(base)
	want := "<naozhi-snapshot at=\"2026-11-04 10:00 UTC\">\n" +
		"会话（在跑 1，最近 24 小时结束 1）：\n" +
		"- 修登录（名字 iso-login，dashboard:direct:a:general） [活跃] · 最后输出 11-04 09:59\n" +
		"- iso-b（dashboard:direct:b:general） [失败·rate_limit] · 最后输出 11-04 09:00\n" +
		"</naozhi-snapshot>"
	if got != want {
		t.Fatalf("snapshot =\n%s\nwant\n%s", got, want)
	}

	withPromises := base
	withPromises.Promises = []Promise{}
	if got := renderSnapshot(withPromises); !strings.Contains(got, "未完成的承诺（0）：") || strings.Contains(got, "等你") {
		t.Errorf("wired-but-empty promises, unwired waiting:\n%s", got)
	}
	// A long promise is cut in its text, never its ID.
	withPromises.Promises = []Promise{{ID: "pr_abcdefghij", Text: strings.Repeat("长", 200), Due: base.Now.Add(time.Hour)}}
	if got := renderSnapshot(withPromises); !strings.Contains(got, "- [pr_abcdefghij] 长长") {
		t.Errorf("long promise lost its ID:\n%s", got)
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

// TestSnapshotListsEveryPromise: the Planner closes a promise by the ID it
// reads in the snapshot, so however many there are and whatever else fills
// the budget, every open promise is listed with its ID; only texts are cut.
func TestSnapshotListsEveryPromise(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		promises int
		text     string
		crowded  bool // waiting and sessions fill the budget too
		cutText  bool // the texts are cut
	}{
		{"few short", 3, "盯着 CI", false, false},
		{"past the old cap", 30, "盯着 CI", false, false},
		{"long texts", 20, strings.Repeat("长", 120), false, true},
		{"crowded", 60, strings.Repeat("长", 120), true, true},
		{"the open limit", 200, strings.Repeat("长", 500), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := snapshotInput{Now: now, Promises: []Promise{}}
			for i := range c.promises {
				p := Promise{ID: fmt.Sprintf("pr_%010d", i), Text: c.text}
				if i%2 == 0 {
					p.Due = now.Add(time.Duration(i) * time.Minute)
				}
				in.Promises = append(in.Promises, p)
			}
			if c.crowded {
				in.Waiting = []Waiting{}
				for i := range 15 {
					in.Waiting = append(in.Waiting, Waiting{Kind: "提问", Title: strings.Repeat("问", 150), Created: now.Add(time.Duration(i) * time.Second)})
				}
				for i := range 50 {
					in.Units = append(in.Units, health.Unit{ID: fmt.Sprintf("dashboard:direct:%03d:general", i), Kind: health.KindSession, State: health.Active, LastActive: now})
				}
			}
			got := renderSnapshot(in)
			head := string([]rune(c.text)[:min(6, utf8.RuneCountInString(c.text))])
			for _, p := range in.Promises {
				if !strings.Contains(got, "- ["+p.ID+"] "+head) {
					t.Fatalf("promise %s missing or its text dropped:\n%s", p.ID, got)
				}
			}
			if full := strings.Contains(got, "] "+c.text); full == c.cutText {
				t.Errorf("texts kept whole = %v, want %v", full, !c.cutText)
			}
			if !strings.Contains(got, "（到期 11-04 10:00）") {
				t.Errorf("due time dropped:\n%s", got)
			}
			if !c.crowded && !strings.Contains(got, "会话（在跑 0") {
				t.Errorf("sessions header cut with room left:\n%s", got)
			}
		})
	}
}

// TestSendRef: a handed-over session leads the message, ahead of the
// snapshot (the logged bubble keeps only the message's head), its fields
// JSON-escaped; it allows an empty text, and an unknown session (or the
// Planner itself) is refused.
func TestSendRef(t *testing.T) {
	now := time.Now()
	ref := `<naozhi-session-ref>{"key":"dashboard:direct:a:general","name":"iso-login","title":"修 \u003c登录\u003e"}</naozhi-session-ref>`
	for _, tc := range []struct {
		text, ref  string
		head, tail string
		err        error
	}{
		{text: "看看这个", head: "<naozhi-snapshot", tail: "</naozhi-snapshot>\n\n看看这个"},
		{text: " 帮我盯着 ", ref: refKey, head: ref + "\n<naozhi-snapshot", tail: "</naozhi-snapshot>\n\n帮我盯着"},
		{ref: refKey, head: ref + "\n<naozhi-snapshot", tail: "</naozhi-snapshot>"},
		{text: "x", ref: "dashboard:direct:gone:general", err: ErrNoRef},
		{text: "x", ref: Key, err: ErrNoRef},
		{text: "  ", err: ErrEmpty},
	} {
		a, _ := newTestAgent(t, &fakeSessions{}, &now)
		turns := &fakeTurns{}
		a.turns = turns
		err := a.Send(tc.text, tc.ref, nil)
		if !errors.Is(err, tc.err) {
			t.Fatalf("Send(%q, %q) = %v, want %v", tc.text, tc.ref, err, tc.err)
		}
		if tc.err != nil {
			continue
		}
		if len(turns.sent) != 1 || !strings.HasPrefix(turns.sent[0], tc.head) || !strings.HasSuffix(turns.sent[0], tc.tail) {
			t.Errorf("Send(%q, %q) sent %q, want %q … %q", tc.text, tc.ref, turns.sent, tc.head, tc.tail)
		}
	}
}

// TestSendImages: the pictures sent with a message reach the turn with the
// snapshot-led text, in order and untouched; with them an empty text is a
// message, and a handed-over session carries them.
func TestSendImages(t *testing.T) {
	now := time.Now()
	imgs := []clievent.Attachment{
		{Kind: clievent.KindImageInline, MimeType: "image/png", Data: []byte("png-1")},
		{Kind: clievent.KindImageInline, MimeType: "image/jpeg", Data: []byte("jpg-2")},
	}
	for _, tc := range []struct {
		name, text, ref string
		src             Source
		tail            string
	}{
		{name: "text and images", text: "看图", src: SourceUser, tail: "</naozhi-snapshot>\n\n看图"},
		{name: "images only", src: SourceUser, tail: "</naozhi-snapshot>"},
		{name: "handed over", text: "这个页面", ref: refKey, src: SourceHandover, tail: "</naozhi-snapshot>\n\n这个页面"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestAgent(t, &fakeSessions{}, &now)
			turns := &fakeTurns{}
			a.turns = turns
			if err := a.Send(tc.text, tc.ref, imgs); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if len(turns.sent) != 1 || !strings.HasSuffix(turns.sent[0], tc.tail) {
				t.Fatalf("sent %q, want … %q", turns.sent, tc.tail)
			}
			if turns.srcs[0] != tc.src {
				t.Errorf("source %q, want %q", turns.srcs[0], tc.src)
			}
			got := turns.images[0]
			if len(got) != len(imgs) {
				t.Fatalf("turn got %d images, want %d", len(got), len(imgs))
			}
			for i := range imgs {
				if got[i].MimeType != imgs[i].MimeType || string(got[i].Data) != string(imgs[i].Data) {
					t.Errorf("image %d = %s %q, want %s %q", i, got[i].MimeType, got[i].Data, imgs[i].MimeType, imgs[i].Data)
				}
			}
		})
	}
	// Without images an empty text is still nothing to send.
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	a.turns = &fakeTurns{}
	if err := a.Send(" ", "", nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Send of nothing = %v, want ErrEmpty", err)
	}
}

// TestTurnSourceJudgement: the turn in flight is judged by every source of
// its requests: outside content names it, and only a receipt of blocks not
// run (outside or not) makes it that receipt's own turn.
func TestTurnSourceJudgement(t *testing.T) {
	for _, tc := range []struct {
		sources []Source
		outside string
		unrun   bool
	}{
		{},
		{sources: []Source{SourceReceipt, SourceUser}},
		{sources: []Source{SourceUnrun, SourceUser}, unrun: true},
		{sources: []Source{SourceRefusal}, outside: "被拒动作的回执", unrun: true},
		{sources: []Source{SourceBriefing, SourceScout}, outside: "会前简报、巡检"},
	} {
		now := time.Now()
		a, _ := newTestAgent(t, &fakeSessions{}, &now)
		a.turns = &fakeTurns{sources: tc.sources}
		if got, unrun := a.OutsideTurn(), a.UnrunTurn(); got != tc.outside || unrun != tc.unrun {
			t.Errorf("%v: outside %q, unrun %v; want %q, %v", tc.sources, got, unrun, tc.outside, tc.unrun)
		}
	}
}

// heldTurns keeps every turn open until the test ends it.
type heldTurns struct {
	fakeTurns
	dones []func(string, error)
}

func (h *heldTurns) SubmitPlanner(_ string, _ Source, _ *Words, text string, _ []clievent.Attachment, _ sessionview.AgentOpts, done func(string, error)) (bool, error) {
	h.sent = append(h.sent, text)
	h.dones = append(h.dones, done)
	return false, nil
}

// TestWakeBatches: the blocks of one wake go one turn each, the next once
// the previous turn ended (a queued block would merge with the one before
// it); one the Planner no longer takes is said in the conversation and
// told to the caller, who keeps it.
func TestWakeBatches(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	h := &heldTurns{}
	a.turns = h
	var sent []string
	told := func(i int, err error) { sent = append(sent, fmt.Sprint(i, " ", err)) }
	if err := a.Wake(SourceScout, told, "<b1>", "<b2>", "<b3>"); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 || !strings.HasPrefix(h.sent[0], "<b1>") {
		t.Fatalf("sent before the first turn ended: %q", h.sent)
	}
	h.dones[0]("", errors.New("turn failed")) // a failed turn still passes on
	if len(h.sent) != 2 || !strings.HasPrefix(h.sent[1], "<b2>") {
		t.Fatalf("after the first turn: %q", h.sent)
	}
	h.dones[0]("", nil) // a second call is no second send
	if len(h.sent) != 2 {
		t.Fatalf("a repeated done sent again: %q", h.sent)
	}
	if err := a.SetStopped(true); err != nil {
		t.Fatal(err)
	}
	h.dones[1]("", nil)
	if len(h.sent) != 2 || len(h.notes) != 1 || !strings.Contains(h.notes[0], "还有 1 批") {
		t.Errorf("stopped mid-chain: sent %d, notes %q", len(h.sent), h.notes)
	}
	if want := []string{"1 <nil>", "2 " + ErrStopped.Error()}; !slices.Equal(sent, want) {
		t.Errorf("told the caller %q, want %q", sent, want)
	}
}

// TestSetStoppedAlive: the switch answers with the outcome, so stopping an
// idle Planner reports it no longer alive; a busy one stays alive until its
// turn is over.
func TestSetStoppedAlive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		alive   bool
	}{
		{"idle", false, false},
		{"mid-turn", true, true},
	} {
		now := time.Now()
		sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, Running: tc.running}}
		a, _ := newTestAgent(t, sess, &now)
		if err := a.SetStopped(true); err != nil {
			t.Fatal(err)
		}
		if st := a.Status(); !st.Stopped || st.Alive != tc.alive {
			t.Errorf("%s: status after stop = %+v, want alive %v", tc.name, st, tc.alive)
		}
	}
}

// TestNotesAcrossRestart: a note shows in the conversation and is journaled,
// so an agent started later lists it; other journal lines are not notes.
func TestNotesAcrossRestart(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Enabled: true, Layout: datadir.FromRoot(t.TempDir()), Sessions: &fakeSessions{}, Journal: j,
		Now: func() time.Time { return now }})
	turns := &fakeTurns{}
	a.turns = turns
	a.Note("已派出：修登录（task:abc）")
	if err := j.Append(journal.Entry{TS: now, Kind: journal.KindHealth, Unit: Key}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	a.Note("到点提醒（答应你的，到期 11-04 10:01）：交周报")
	if !slices.Equal(turns.notes, []string{"已派出：修登录（task:abc）", "到点提醒（答应你的，到期 11-04 10:01）：交周报"}) {
		t.Errorf("shown = %q", turns.notes)
	}

	restarted := New(Config{Enabled: true, Layout: datadir.FromRoot(t.TempDir()), Sessions: &fakeSessions{}, Journal: j,
		Now: func() time.Time { return now.Add(time.Hour) }})
	notes, err := restarted.Notes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || notes[0].Text != "已派出：修登录（task:abc）" || !notes[1].Time.Equal(now) {
		t.Errorf("notes after restart = %+v", notes)
	}
}

// TestPromptUpgradeChangesShift: a conversation started on an older system
// prompt changes shift at once (a resumed one keeps its first prompt), under
// its own handoff name, and only once; a dead Planner just records the new
// version when its fresh conversation starts.
func TestPromptUpgradeChangesShift(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 4, 21, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1", Running: true}}
	a, lay := newTestAgent(t, sess, &now)
	turns := &fakeTurns{reply: "## 进行中\n统计任务"}
	a.turns = turns
	a.mu.Lock()
	a.state.Shift, a.state.Prompt = shiftDay(now), PromptVersion-1
	a.mu.Unlock()
	ctx := context.Background()

	a.reconcile(ctx) // waits out a running turn
	if sess.recreated != 0 {
		t.Fatalf("changed shift during a turn")
	}
	sess.mu.Lock()
	sess.st.Running = false
	sess.mu.Unlock()
	a.reconcile(ctx)
	if sess.recreated != 1 {
		t.Fatalf("recreated %d, want 1", sess.recreated)
	}
	name := fmt.Sprintf("%s-v%d.md", shiftDay(now), PromptVersion)
	if saved, err := os.ReadFile(filepath.Join(lay.PlannerHandoffRoot(), name)); err != nil || string(saved) != turns.reply {
		t.Fatalf("handoff %s = %q, %v", name, saved, err)
	}
	a.reconcile(ctx)
	if sess.recreated != 1 {
		t.Fatalf("changed shift again: recreated %d", sess.recreated)
	}
	a.mu.Lock()
	got := a.state.Prompt
	a.mu.Unlock()
	if got != PromptVersion {
		t.Errorf("prompt = %d, want %d", got, PromptVersion)
	}
}

// TestPromptUpgradeChangesShiftWithUnknownSessionID: a live Planner adopted
// from a shim before its first turn has no session ID yet but still runs on
// the old prompt, so it changes shift too.
func TestPromptUpgradeChangesShiftWithUnknownSessionID(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 5, 19, 22, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true}}
	a, _ := newTestAgent(t, sess, &now)
	a.turns = &fakeTurns{reply: "## 进行中\n无"}
	a.mu.Lock()
	a.state.Shift, a.state.Prompt = shiftDay(now), PromptVersion-1
	a.mu.Unlock()
	a.reconcile(context.Background())
	if sess.recreated != 1 {
		t.Fatalf("recreated %d, want 1", sess.recreated)
	}
}

// TestSendWords: a user message's turn records the user's own words (no
// snapshot, no ref), a receipt's records none, and the answer to a question
// records the answer from SourceAnswer (D98).
func TestSendWords(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	turns := &fakeTurns{}
	a.turns = turns
	if err := a.Send(" 都合并 ", refKey, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Send("", refKey, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Tell("已派出"); err != nil {
		t.Fatal(err)
	}
	if err := a.TellAnswer("用户的回答：发", Words{Text: "发", Question: "发版吗？"}); err != nil {
		t.Fatal(err)
	}
	if len(turns.said) != 4 || turns.said[0] == nil || turns.said[0].Text != "都合并" || turns.said[1] != nil || turns.said[2] != nil ||
		turns.said[3] == nil || turns.said[3].Text != "发" || turns.said[3].Question != "发版吗？" {
		t.Errorf("said = %+v", turns.said)
	}
	if !slices.Equal(turns.srcs, []Source{SourceHandover, SourceHandover, SourceReceipt, SourceAnswer}) {
		t.Errorf("sources = %v", turns.srcs)
	}
	if SourceAnswer.Outside() {
		t.Error("an answer's turn reads as outside content")
	}
	turns.words = []Words{{Text: "x"}}
	if w := a.TurnWords(); len(w) != 1 || w[0].Text != "x" {
		t.Errorf("TurnWords = %+v", w)
	}
}

// TestSnapshotUnchanged: a message whose snapshot says what the last full
// one in this conversation said carries only a mark naming that one's time
// and the time now (the Planner's only clock); a change, a new conversation
// (shift change, another session ID) or a process that is not running (its
// revival may not resume) gets it whole.
func TestSnapshotUnchanged(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, _ := newTestAgent(t, sess, &now)
	turns := &fakeTurns{reply: "## 进行中\n无"}
	a.turns = turns
	units := []health.Unit{{ID: "dashboard:direct:x:general", State: health.Active, LastActive: now}}
	a.health = func() health.Snapshot { return health.Snapshot{Units: slices.Clone(units)} }
	a.markShift(now)
	const full = `<naozhi-snapshot at="`
	send := func(at time.Time, text string) string {
		t.Helper()
		now = at
		if err := a.Send(text, "", nil); err != nil {
			t.Fatal(err)
		}
		return turns.sent[len(turns.sent)-1]
	}
	at := func(hm string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", hm, loc)
		return v
	}

	if got := send(at("2026-11-03 10:00"), "a"); !strings.HasPrefix(got, full) {
		t.Fatalf("first message: %q", got)
	}
	if got := send(at("2026-11-03 10:05"), "b"); got != `<naozhi-snapshot unchanged since="10:00" at="2026-11-03 10:05 CST"/>`+"\n\nb" {
		t.Fatalf("same fleet: %q", got)
	}
	units = append(units, health.Unit{ID: "task:ab12", State: health.Active, LastActive: now})
	if got := send(at("2026-11-03 10:10"), "c"); !strings.HasPrefix(got, full) || !strings.Contains(got, "task:ab12") {
		t.Fatalf("changed fleet: %q", got)
	}
	if got := send(at("2026-11-03 10:20"), "d"); !strings.HasPrefix(got, `<naozhi-snapshot unchanged since="10:10" at="2026-11-03 10:20 CST"/>`) {
		t.Fatalf("unchanged since the change: %q", got)
	}
	if err := a.Wake(SourceScout, nil, "<naozhi-scout>x</naozhi-scout>"); err != nil {
		t.Fatal(err)
	}
	if got := turns.sent[len(turns.sent)-1]; !strings.HasSuffix(got, `<naozhi-snapshot unchanged since="10:10" at="2026-11-03 10:20 CST"/>`) {
		t.Fatalf("wake: %q", got)
	}

	// The CLI named another conversation.
	sess.st.SessionID = "s2"
	if got := send(at("2026-11-03 10:30"), "e"); !strings.HasPrefix(got, full) {
		t.Fatalf("another session ID: %q", got)
	}
	// Not running: the message spawns it.
	sess.st.Alive = false
	if got := send(at("2026-11-03 10:40"), "f"); !strings.HasPrefix(got, full) {
		t.Fatalf("process not running: %q", got)
	}
	// Revived by the lifecycle, maybe without its conversation.
	sess.st.Alive = false
	now = at("2026-11-03 10:45")
	a.reconcile(context.Background())
	if !sess.st.Alive {
		t.Fatal("not revived")
	}
	if got := send(at("2026-11-03 10:50"), "g"); !strings.HasPrefix(got, full) {
		t.Fatalf("after revival: %q", got)
	}
	if got := send(at("2026-11-03 10:55"), "h"); !strings.HasPrefix(got, `<naozhi-snapshot unchanged since="10:50" at="2026-11-03 10:55 CST"/>`) {
		t.Fatalf("after the revival's full one: %q", got)
	}

	// The shift change starts a conversation that has seen none, its ID
	// unknown until its first turn, like one the CLI had not named when the
	// last full snapshot went out.
	sess.st.SessionID = ""
	if got := send(at("2026-11-03 11:00"), "i"); !strings.HasPrefix(got, full) {
		t.Fatalf("unnamed conversation: %q", got)
	}
	now = at("2026-11-04 08:30")
	a.reconcile(context.Background())
	if sess.recreated != 1 || sess.st.SessionID != "" {
		t.Fatalf("recreated %d, state %+v", sess.recreated, sess.st)
	}
	if got := send(at("2026-11-04 08:40"), "j"); !strings.HasPrefix(got, full) {
		t.Fatalf("after the shift change: %q", got)
	}
}

// TestSnapshotUnchangedWhileRunning: a running session's 最后输出 moves with
// its events, and the order of running sessions with it; neither is a
// change, so a message still carries only the mark. The full snapshot still
// says 最后输出, and a state change is a change.
func TestSnapshotUnchangedWhileRunning(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, loc)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, _ := newTestAgent(t, sess, &now)
	turns := &fakeTurns{reply: "ok"}
	a.turns = turns
	units := []health.Unit{
		{ID: "task:aa", State: health.Active, LastActive: now},
		{ID: "task:bb", State: health.Active, LastActive: now.Add(-time.Minute)},
	}
	a.health = func() health.Snapshot { return health.Snapshot{Units: slices.Clone(units)} }
	a.markShift(now)
	send := func(text string) string {
		t.Helper()
		now = now.Add(3 * time.Minute)
		if err := a.Send(text, "", nil); err != nil {
			t.Fatal(err)
		}
		return turns.sent[len(turns.sent)-1]
	}
	if got := send("a"); !strings.Contains(got, "task:aa [活跃] · 最后输出 11-03 10:00") {
		t.Fatalf("full snapshot: %q", got)
	}
	units[0].LastActive = now.Add(2 * time.Minute)
	if got := send("b"); !strings.HasPrefix(got, "<naozhi-snapshot unchanged") {
		t.Fatalf("only 最后输出 moved: %q", got)
	}
	units[1].LastActive = now.Add(time.Minute) // task:bb now leads the list
	if got := send("c"); !strings.HasPrefix(got, "<naozhi-snapshot unchanged") {
		t.Fatalf("only the order moved: %q", got)
	}
	units[0].State = health.Waiting
	got := send("d")
	if !strings.HasPrefix(got, `<naozhi-snapshot at="`) || !strings.Contains(got, "task:aa [待你回复] · 最后输出 11-03 10:05") {
		t.Fatalf("a state change: %q", got)
	}
}

// TestSnapshotMarkOnlyWhenSafe: the mark goes only where the last full
// snapshot is surely in the conversation the message goes into. A full one
// counts once its turn ran (one dropped from the queue or failed may never
// have reached the CLI), in the conversation named then; a message to a
// busy Planner is queued, and may reach another conversation by its turn;
// a compaction may have summarised the last one away.
func TestSnapshotMarkOnlyWhenSafe(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	const full, mark = `<naozhi-snapshot at="`, `<naozhi-snapshot unchanged since="10:05" at="2026-11-03 10:10 CST"/>`
	setup := func(t *testing.T) (*Agent, *fakeSessions, *fakeTurns, func(string) string) {
		now := time.Date(2026, 11, 3, 10, 0, 0, 0, loc)
		sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
		a, _ := newTestAgent(t, sess, &now)
		turns := &fakeTurns{reply: "ok"}
		a.turns = turns
		last := now
		a.health = func() health.Snapshot {
			return health.Snapshot{Units: []health.Unit{{ID: "dashboard:direct:x:general", State: health.Idle, LastActive: last}}}
		}
		a.markShift(now)
		send := func(text string) string {
			t.Helper()
			now = now.Add(5 * time.Minute)
			if err := a.Send(text, "", nil); err != nil {
				t.Fatal(err)
			}
			return turns.sent[len(turns.sent)-1]
		}
		return a, sess, turns, send
	}

	t.Run("its turn never ran", func(t *testing.T) {
		_, _, turns, send := setup(t)
		turns.turnErr = errors.New("消息未被处理")
		send("a")
		turns.turnErr = nil
		if got := send("b"); !strings.HasPrefix(got, full) {
			t.Fatalf("after a dropped full one: %q", got)
		}
		if got := send("c"); !strings.HasPrefix(got, "<naozhi-snapshot unchanged") {
			t.Fatalf("after one that ran: %q", got)
		}
	})
	t.Run("busy Planner", func(t *testing.T) {
		_, sess, _, send := setup(t)
		send("a")
		sess.st.Running = true
		if got := send("b"); !strings.HasPrefix(got, full) {
			t.Fatalf("queued behind a turn: %q", got)
		}
	})
	t.Run("conversation unnamed when it ran", func(t *testing.T) {
		_, sess, _, send := setup(t)
		sess.st.SessionID = ""
		send("a")
		if got := send("b"); !strings.HasPrefix(got, full) {
			t.Fatalf("still unnamed: %q", got)
		}
		sess.st.SessionID = "s9" // another conversation, maybe
		if got := send("c"); !strings.HasPrefix(got, full) {
			t.Fatalf("named since: %q", got)
		}
	})
	t.Run("compacted", func(t *testing.T) {
		_, sess, _, send := setup(t)
		send("a")
		if got := send("b"); got != mark+"\n\nb" {
			t.Fatalf("not compacted: %q", got)
		}
		sess.compacted = true
		if got := send("c"); !strings.HasPrefix(got, full) {
			t.Fatalf("compacted since: %q", got)
		}
	})
	t.Run("forgotten while its turn ran", func(t *testing.T) {
		a, _, turns, send := setup(t)
		var ran func(string, error)
		held := &holdTurns{fakeTurns: turns, held: &ran}
		a.turns = held
		send("a")
		a.forgetSnapshot() // a shift change, a revival
		ran("ok", nil)
		a.turns = turns
		if got := send("b"); !strings.HasPrefix(got, full) {
			t.Fatalf("a turn submitted before the forget recorded its snapshot: %q", got)
		}
	})
	t.Run("sent between the handoff and the new conversation", func(t *testing.T) {
		a, sess, turns, send := setup(t)
		send("a")
		// The first one's turn runs on the old session and ends at once; the
		// second, sent while the Planner is idle on that session's ID, still
		// goes into the new conversation.
		var got string
		a.turns = &interjectTurns{fakeTurns: turns, after: handoffPrompt, do: func() {
			for _, text := range []string{"插话", "再插一句"} {
				if err := a.Send(text, "", nil); err != nil {
					t.Error(err)
				}
			}
			got = turns.sent[len(turns.sent)-1]
		}}
		a.changeShift(context.Background(), a.cfg.Now(), "2026-11-04")
		if sess.recreated != 1 {
			t.Fatalf("recreated %d", sess.recreated)
		}
		if !strings.HasPrefix(got, full) {
			t.Fatalf("a message decided on the old conversation may open the new one: %q", got)
		}
	})
	t.Run("its turn ended after the shift change", func(t *testing.T) {
		a, sess, turns, send := setup(t)
		var ran func(string, error)
		a.turns = &holdTurns{fakeTurns: turns, held: &ran}
		send("a")
		a.changeShift(context.Background(), a.cfg.Now(), "2026-11-04")
		if sess.recreated != 1 {
			t.Fatalf("recreated %d", sess.recreated)
		}
		sess.st.SessionID = "s2" // the new conversation, named
		ran("ok", nil)
		a.turns = turns
		if got := send("b"); !strings.HasPrefix(got, full) {
			t.Fatalf("the old conversation's snapshot was taken for the new one's: %q", got)
		}
	})
}

// interjectTurns is fakeTurns that runs do once the turn of the message
// after has ended, as a message arriving then would.
type interjectTurns struct {
	*fakeTurns
	after string
	do    func()
}

func (f *interjectTurns) SubmitPlanner(key string, src Source, said *Words, text string, images []clievent.Attachment, opts sessionview.AgentOpts, done func(string, error)) (bool, error) {
	q, err := f.fakeTurns.SubmitPlanner(key, src, said, text, images, opts, done)
	if text == f.after {
		f.do()
	}
	return q, err
}

// holdTurns is fakeTurns whose user turns end only when the test calls
// held; the gateway's own (the handoff) end at once.
type holdTurns struct {
	*fakeTurns
	held *func(string, error)
}

func (h *holdTurns) SubmitPlanner(key string, src Source, said *Words, text string, images []clievent.Attachment, opts sessionview.AgentOpts, done func(string, error)) (bool, error) {
	if src != SourceUser {
		return h.fakeTurns.SubmitPlanner(key, src, said, text, images, opts, done)
	}
	*h.held = done
	return h.fakeTurns.SubmitPlanner(key, src, said, text, images, opts, nil)
}
