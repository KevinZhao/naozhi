package wireup

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session"
)

// newDigestRouter is a router for these tests. The backends are registered
// first, as main does: a snapshot would register them lazily otherwise, and
// RegisterCLIBackends (TestHistoryBackends_EveryProfileHasFactory) would then
// register them twice.
func newDigestRouter(t *testing.T) *session.Router {
	RegisterCLIBackends()
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	return r
}

func ev(ms int64, typ, text string) clievent.EventEntry {
	return clievent.EventEntry{Time: ms, Type: typ, Summary: text, Detail: text}
}

// TestDigestFacts gathers a digest from the router, health, sidecars,
// outputs and inbox: who is covered, the goal seen, the replies by turn,
// the open questions (the CLI's card and the Planner's asks for the task).
func TestDigestFacts(t *testing.T) {
	r := newDigestRouter(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := workspace.Load("", "node1")
	ws, _ := reg.Ensure(dir)
	sc := sidecar.Open(t.TempDir(), nil)

	const task, chat, cronKey, plannerKey = "task:aaaaaaaaaaaa", "dashboard:direct:u:general", "cron:j1", "planner:main"
	if err := sc.Put(sidecar.Meta{Key: task, Purpose: sidecar.PurposeTask, Title: "派出的标题"}); err != nil {
		t.Fatal(err)
	}
	tp := session.NewTestProcess()
	for _, e := range []clievent.EventEntry{
		{Time: 1000, Type: clievent.KindUser, Source: clievent.SourcePlanner, Detail: "把 X 修好\n\n细节\n\n（网关附注：…）"},
		ev(1100, clievent.KindText, "第一段"), ev(1200, clievent.KindText, "回合一的结论"), ev(1300, clievent.KindResult, ""),
		{Time: 1400, Type: clievent.KindUser, Source: clievent.SourceCustody, Detail: "继续"},
		ev(1500, clievent.KindText, "回合二"), ev(1600, clievent.KindResult, ""),
		ev(1700, clievent.KindText, "回合三"), ev(1800, clievent.KindResult, ""),
		ev(1900, clievent.KindText, "回合四"), ev(2000, clievent.KindResult, ""),
		ev(2100, clievent.KindText, "还没结束的回合"),
		{Time: 2200, Type: clievent.KindAskQuestion, Summary: "card", AskQuestion: &clievent.AskQuestion{Items: []clievent.AskQuestionItem{{Question: "选哪个？"}}}},
	} {
		tp.EventLog.Append(e)
	}
	r.InjectSession(task, tp).SetWorkspaceForTest(dir)
	cp := session.NewTestProcess()
	cp.EventLog.Append(clievent.EventEntry{Time: 1000, Type: clievent.KindUser, Source: clievent.SourceRelay, Detail: "转达的"})
	cp.EventLog.Append(clievent.EventEntry{Time: 1100, Type: clievent.KindUser, Detail: "帮我看看 trn2"})
	r.InjectSession(chat, cp)
	r.InjectSession(cronKey, session.NewTestProcess())
	r.InjectSession(plannerKey, session.NewTestProcess())

	rec := activity.NewRecorder(nil, nil)
	rec.Record(task, dir, activity.Activity{Type: activity.TypeOutput, Summary: "r.md", Path: "r.md"})
	payload, _ := json.Marshal(map[string]string{"question": "合并吗？", "task": task})
	inbox := []notify.Item{
		{Kind: notify.KindDecision, State: notify.StateOpen, Unit: plannerKey, Title: "合并吗？", Decision: &notify.Decision{Handler: action.HandlerAsk, Payload: payload}},
		{Kind: notify.KindQuestion, State: notify.StateOpen, Unit: task, Title: "要你决定"},
		{Kind: notify.KindError, State: notify.StateOpen, Unit: task, Title: "出错了"},
		{Kind: notify.KindQuestion, State: notify.StateClosed, Unit: task, Title: "已关"},
	}
	since := time.UnixMilli(5000)
	src := DigestSources{
		Router: r, Sidecars: sc, Outputs: rec, Workspaces: reg,
		Health: func() health.Snapshot {
			return health.Snapshot{Units: []health.Unit{{ID: task, State: health.Waiting, Since: since}}}
		},
		Inbox: func() []notify.Item { return inbox },
	}

	f, ok := src.Facts(task)
	if !ok {
		t.Fatal("task not covered")
	}
	if f.Title != "派出的标题" || f.Purpose != sidecar.PurposeTask || f.Workspace != ws.Name || f.Dir != dir ||
		f.Health != health.Waiting || !f.HealthSince.Equal(since) || f.GoalSeen != "把 X 修好\n\n细节\n\n（网关附注：…）" {
		t.Errorf("task facts = %+v", f)
	}
	if got := digest.Goal(f.GoalSeen); got != "把 X 修好" {
		t.Errorf("goal = %q", got)
	}
	var replies []string
	for _, rp := range f.Replies {
		replies = append(replies, rp.Text)
	}
	if !slices.Equal(replies, []string{"回合二", "回合三", "回合四"}) {
		t.Errorf("replies = %q", replies)
	}
	if len(f.Outputs) != 1 || f.Outputs[0].Path != "r.md" {
		t.Errorf("outputs = %+v", f.Outputs)
	}
	if !slices.Equal(f.Questions, []string{"选哪个？", "合并吗？", "要你决定"}) {
		t.Errorf("questions = %q", f.Questions)
	}

	f, ok = src.Facts(chat)
	if !ok || f.Purpose != sidecar.PurposeChat || f.GoalSeen != "帮我看看 trn2" {
		t.Errorf("chat facts = %+v, %v", f, ok)
	}
	for _, k := range []string{cronKey, plannerKey, "scout:r1", "scratch:x", "task:gone00000000"} {
		if _, ok := src.Facts(k); ok {
			t.Errorf("%s covered", k)
		}
	}
	keys := src.Keys()
	slices.Sort(keys)
	if !slices.Equal(keys, []string{chat, task}) {
		t.Errorf("keys = %q", keys)
	}
	// Observing turned off leaves a chat out, never a task.
	src.Watched = func(string) bool { return false }
	if _, ok := src.Facts(chat); ok {
		t.Error("unobserved chat covered")
	}
	if _, ok := src.Facts(task); !ok {
		t.Error("task left out with observing off")
	}
	if src.Observed(task) {
		t.Error("a task counts as observed")
	}
}

// TestDigestIndex: index.json rows carry the digest path and the report's
// first sentence, and an observed session's purpose is "chat".
func TestDigestIndex(t *testing.T) {
	r := newDigestRouter(t)
	const chat, cronKey = "dashboard:direct:u:general", "cron:j1"
	r.InjectSession(chat, session.NewTestProcess())
	r.InjectSession(cronKey, session.NewTestProcess())
	dir := t.TempDir()
	src := DigestSources{Router: r}
	w := digest.New(digest.Config{Dir: dir, Facts: src.Facts})
	if err := w.SetReport(chat, digest.Report{State: "progress", Summary: "跑到一半。还要两小时", At: time.UnixMilli(1000)}); err != nil {
		t.Fatal(err)
	}
	h := HealthSources{Router: r, Digests: w, Observed: src.Observed}
	rows := map[string]*health.IndexEntry{}
	for _, f := range h.Collect(time.Time{}) {
		if f.Index != nil {
			rows[f.Index.Key] = f.Index
		}
	}
	e := rows[chat]
	if e == nil || e.Purpose != sidecar.PurposeChat || e.Report == nil || e.Report.Summary != "跑到一半。" || e.Report.State != "progress" {
		t.Fatalf("chat row = %+v", e)
	}
	if e.Digest != "" {
		t.Errorf("digest path before one was written = %q", e.Digest)
	}
	if c := rows[cronKey]; c == nil || c.Purpose != "" || c.Report != nil {
		t.Errorf("cron row = %+v", c)
	}
}

// TestDigestSink: a turn's end is recorded and asks for the digest; a
// health move of a session asks too.
func TestDigestSink(t *testing.T) {
	r := newDigestRouter(t)
	const chat = "dashboard:direct:u:general"
	tp := session.NewTestProcess()
	tp.EventLog.Append(ev(1000, clievent.KindText, "答复"))
	tp.EventLog.Append(ev(1100, clievent.KindResult, ""))
	r.InjectSession(chat, tp)
	dir := t.TempDir()
	src := DigestSources{Router: r}
	written := make(chan string, 4)
	w := digest.New(digest.Config{Dir: dir, Facts: src.Facts, MinGap: time.Millisecond, Written: func(k string) { written <- k }})
	ctx := t.Context()
	go w.Run(ctx)
	DigestSink{Recorder: activity.NewRecorder(nil, nil), Digests: w}.RecordTurn(chat, "", activity.TurnDone)
	select {
	case k := <-written:
		if k != chat {
			t.Fatalf("written %q", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn end wrote no digest")
	}
	_, md, ok := w.Read(chat)
	if !ok || !strings.Contains(string(md), "> 答复") {
		t.Fatalf("digest = %s", md)
	}
	tp.EventLog.Append(ev(1200, clievent.KindText, "又一轮"))
	tp.EventLog.Append(ev(1300, clievent.KindResult, ""))
	DigestOnHealth(w)(health.Event{Transition: &health.Transition{Unit: health.Unit{ID: chat, Kind: health.KindSession}}})
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("health move wrote no digest")
	}
}
