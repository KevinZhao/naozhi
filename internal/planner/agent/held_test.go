package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// heldAgent is an agent on a live, idle Planner whose shift is marked, so
// a reconcile reaches the held receipts.
func heldAgent(t *testing.T, now *time.Time) (*Agent, *fakeSessions, *fakeTurns) {
	t.Helper()
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, _ := newTestAgent(t, sess, now)
	turns := &fakeTurns{}
	a.turns = turns
	a.markShift(*now)
	return a, sess, turns
}

// TestTellLaterWhileChatting (D114): a progress-class receipt while the
// user sent a Planner message in the last 5 minutes waits; it goes merged
// with the others in one turn once they have been quiet 5 minutes and the
// Planner is idle, not before, and with none waiting goes at once.
func TestTellLaterWhileChatting(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, loc)
	a, sess, turns := heldAgent(t, &now)

	if err := a.TellLater("已派出：甲"); err != nil {
		t.Fatal(err)
	}
	if len(turns.sent) != 1 || !strings.Contains(turns.sent[0], "已派出：甲") || turns.srcs[0] != SourceReceipt {
		t.Fatalf("with the user away: sent %q, want it at once", turns.sent)
	}
	turns.sent, turns.srcs = nil, nil

	if err := a.Send("看下进度", "", nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := a.TellLater("乙 汇报：进度 · 「跑了一半」"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := a.TellLater("已派出：丙"); err != nil {
		t.Fatal(err)
	}
	if len(turns.sent) != 1 {
		t.Fatalf("while chatting: sent %q, want only the user's message", turns.sent)
	}
	ctx := context.Background()
	now = now.Add(2 * time.Minute) // 4 minutes after the message
	a.reconcile(ctx)
	if len(turns.sent) != 1 {
		t.Fatalf("4 minutes after the message: sent %q", turns.sent)
	}
	now = now.Add(2 * time.Minute)
	sess.mu.Lock()
	sess.st.Running = true
	sess.mu.Unlock()
	a.reconcile(ctx)
	if len(turns.sent) != 1 {
		t.Fatalf("with a turn running: sent %q", turns.sent)
	}
	sess.mu.Lock()
	sess.st.Running = false
	sess.mu.Unlock()
	a.reconcile(ctx)
	if len(turns.sent) != 2 || turns.srcs[1] != SourceReceipt {
		t.Fatalf("quiet and idle: sent %q, want one receipt", turns.sent)
	}
	got := turns.sent[1]
	for _, want := range []string{"攒着的回执", "（14:01）乙 汇报：进度", "（14:02）已派出：丙"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged receipt lacks %q: %s", want, got)
		}
	}
	if strings.Index(got, "乙 汇报") > strings.Index(got, "已派出：丙") {
		t.Errorf("held receipts out of order: %s", got)
	}
	a.reconcile(ctx)
	if len(turns.sent) != 2 {
		t.Errorf("sent again: %q", turns.sent)
	}
}

// TestHeldRideOnReceipt: an immediate receipt (a done report) takes the
// ones waiting along, ahead of it, in one turn; a line kept with TellAlong
// starts no turn of its own, even once the user is quiet.
func TestHeldRideOnReceipt(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	a.TellAlong("甲 汇报：完成 · 「好了」。这份汇报它已经用 SendMessage 发给过你")
	now = now.Add(10 * time.Minute)
	a.reconcile(context.Background())
	if err := a.Tell(""); err != nil || len(turns.sent) != 0 {
		t.Fatalf("a line along started a turn: %q, %v", turns.sent, err)
	}
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.TellLater("已派出：乙"); err != nil {
		t.Fatal(err)
	}
	if err := a.Tell("丙 汇报：完成 · 「合了」"); err != nil {
		t.Fatal(err)
	}
	if len(turns.sent) != 2 {
		t.Fatalf("sent %q, want the message and one receipt", turns.sent)
	}
	got := turns.sent[1]
	a1, b, c := strings.Index(got, "甲 汇报"), strings.Index(got, "已派出：乙"), strings.Index(got, "丙 汇报")
	if a1 < 0 || b < a1 || c < b {
		t.Errorf("receipt = %s, want the lines along, the held one, then its own", got)
	}
	if len(a.state.Held) != 0 {
		t.Errorf("held after the receipt: %+v", a.state.Held)
	}
	if err := a.Tell(""); err != nil || len(turns.sent) != 2 {
		t.Errorf("an empty receipt with none waiting sent %q, %v", turns.sent, err)
	}
}

// TestHeldAcrossRestart: the receipts waiting are in agent.json, so a
// restart loses none; the next run, not knowing when the user last wrote,
// sends them at its first idle reconcile.
func TestHeldAcrossRestart(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.TellLater("已派出：乙"); err != nil {
		t.Fatal(err)
	}
	a.TellAlong("甲 汇报：完成")

	b := New(a.cfg)
	if len(b.state.Held) != 2 {
		t.Fatalf("held after the restart: %+v", b.state.Held)
	}
	b.turns = turns
	turns.sent, turns.srcs = nil, nil
	b.reconcile(context.Background())
	if len(turns.sent) != 1 || !strings.Contains(turns.sent[0], "已派出：乙") || !strings.Contains(turns.sent[0], "甲 汇报：完成") {
		t.Fatalf("after the restart: sent %q", turns.sent)
	}
	if c := New(a.cfg); len(c.state.Held) != 0 {
		t.Errorf("held once sent: %+v", c.state.Held)
	}
}

// TestHeldRefusedWhenStopped: a stopped Planner takes no receipt, held or
// not, as Tell; and the waiting list is bounded.
func TestHeldRefusedWhenStopped(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	for i := range heldMax + 5 {
		if err := a.TellLater("已派出：" + string(rune('a'+i%26))); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.state.Held) != heldMax {
		t.Errorf("held %d, want the cap %d", len(a.state.Held), heldMax)
	}
	a.mu.Lock()
	a.state.Stopped = true
	a.mu.Unlock()
	if err := a.TellLater("已派出：x"); err != ErrStopped {
		t.Errorf("TellLater on a stopped Planner = %v", err)
	}
	a.TellAlong("y")
	if len(a.state.Held) != heldMax || slices.ContainsFunc(a.state.Held, func(h heldReceipt) bool { return h.Text == "y" }) {
		t.Errorf("held while stopped: %d", len(a.state.Held))
	}
	if len(turns.sent) != 1 {
		t.Errorf("sent %q", turns.sent)
	}
}

// hookTurns is fakeTurns, safe across goroutines, running during inside
// each submit: what lands between a Tell reading the held receipts and
// dropping them.
type hookTurns struct {
	mu sync.Mutex
	*fakeTurns
	during func()
}

func (h *hookTurns) SubmitPlanner(key string, src Source, said *Words, text string, images []clievent.Attachment, opts sessionview.AgentOpts, done func(string, error)) (bool, error) {
	if h.during != nil {
		h.during()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fakeTurns.SubmitPlanner(key, src, said, text, images, opts, done)
}

// TestHeldDropOnlySent: a Tell drops the held receipts it sent, not as
// many from the front: one held while it submitted stays, though the list
// lost its oldest past heldMax meanwhile.
func TestHeldDropOnlySent(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	a, _, _ := heldAgent(t, &now)
	turns := &hookTurns{fakeTurns: &fakeTurns{}}
	a.turns = turns
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	for i := range heldMax {
		if err := a.TellLater(fmt.Sprintf("已派出：%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	turns.during = func() {
		turns.during = nil
		if err := a.TellLater("已派出：新"); err != nil {
			t.Error(err)
		}
	}
	if err := a.Tell("甲 汇报：完成"); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Held) != 1 || a.state.Held[0].Text != "已派出：新" {
		t.Fatalf("held = %+v, want the one held during the submit", a.state.Held)
	}
	if strings.Contains(turns.sent[len(turns.sent)-1], "已派出：新") {
		t.Errorf("the receipt carried one held after it was read")
	}
}

// TestHeldConcurrentTells: Tells at once send each held receipt once.
func TestHeldConcurrentTells(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	a, _, _ := heldAgent(t, &now)
	turns := &hookTurns{fakeTurns: &fakeTurns{}}
	a.turns = turns
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.TellLater("已派出：甲"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 8 {
		wg.Go(func() {
			<-start
			if err := a.Tell(fmt.Sprintf("乙%d 汇报：完成", i)); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	n := 0
	for _, s := range turns.sent {
		n += strings.Count(s, "已派出：甲")
	}
	if n != 1 || len(a.state.Held) != 0 {
		t.Errorf("held receipt sent %d times, held after: %+v", n, a.state.Held)
	}
}

const (
	watchKeyA = "dashboard:direct:u:general"
	watchKeyB = "dashboard:direct:u:review"
)

func watchLine(key, last string) string { return `{"key":"` + key + `","last":"` + last + `"}` }

// heldWatch is the watch lines waiting, as key=text.
func heldWatch(a *Agent) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, h := range a.state.Held {
		if h.Watch {
			out = append(out, h.Key+"="+h.Text)
		}
	}
	return out
}

// TestTellWatchAtOnce (D138): with the user not chatting and the Planner
// idle, a watched session's turn end starts a turn from SourceWatch at once,
// its text the <naozhi-watch> block and no receipt's; one of a session that
// read outside content is from SourceWatchOutside. Nothing stays waiting.
func TestTellWatchAtOnce(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	if err := a.TellWatch(watchKeyA, watchLine(watchKeyA, "改好了"), false); err != nil {
		t.Fatal(err)
	}
	if err := a.TellWatch(watchKeyB, watchLine(watchKeyB, "读完了网页"), true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"<naozhi-watch>\n" + watchLine(watchKeyA, "改好了") + "\n</naozhi-watch>",
		"<naozhi-watch>\n" + watchLine(watchKeyB, "读完了网页") + "\n</naozhi-watch>",
	}
	if !slices.Equal(turns.sent, want) || !slices.Equal(turns.srcs, []Source{SourceWatch, SourceWatchOutside}) {
		t.Fatalf("sent %q from %q, want %q from watch, watch_outside", turns.sent, turns.srcs, want)
	}
	if turns.said[0] != nil || turns.said[1] != nil {
		t.Errorf("a watch turn carried the user's words: %v", turns.said)
	}
	if got := heldWatch(a); len(got) != 0 {
		t.Errorf("held after sending: %q", got)
	}
}

// TestTellWatchWhileChatting (D114, D138): while the user sent a Planner
// message in the last 5 minutes the watch lines wait, a session's later
// line replacing its earlier one; a receipt going meanwhile does not take
// them along, so they are never answered in a turn that may dispatch. Once
// the user has been quiet 5 minutes and the Planner is idle they go: the
// sessions that read outside content in one turn, the rest in another.
func TestTellWatchWhileChatting(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	const keyC, keyD = "dashboard:direct:u:docs", "feishu:direct:alice:general"
	for _, w := range []struct {
		key, last string
		outside   bool
	}{
		{watchKeyA, "第一轮", false},
		{watchKeyB, "查了网页", true},
		{watchKeyA, "第二轮", false},
		{keyC, "文档写完了", false},
		{keyD, "读了邮件", true},
	} {
		now = now.Add(10 * time.Second)
		if err := a.TellWatch(w.key, watchLine(w.key, w.last), w.outside); err != nil {
			t.Fatal(err)
		}
	}
	if len(turns.sent) != 1 {
		t.Fatalf("while chatting: sent %q, want only the user's message", turns.sent)
	}
	wantHeld := []string{
		watchKeyB + "=" + watchLine(watchKeyB, "查了网页"), watchKeyA + "=" + watchLine(watchKeyA, "第二轮"),
		keyC + "=" + watchLine(keyC, "文档写完了"), keyD + "=" + watchLine(keyD, "读了邮件"),
	}
	if got := heldWatch(a); !slices.Equal(got, wantHeld) {
		t.Fatalf("held = %q, want %q", got, wantHeld)
	}
	// A receipt of the gateway's own goes at once and leaves them waiting.
	if err := a.Tell("派出「甲」失败"); err != nil {
		t.Fatal(err)
	}
	if len(turns.sent) != 2 || turns.srcs[1] != SourceReceipt || strings.Contains(turns.sent[1], "naozhi-watch") || strings.Contains(turns.sent[1], watchKeyA) {
		t.Fatalf("receipt = %q from %q, want it alone", turns.sent[1:], turns.srcs[1:])
	}
	if got := heldWatch(a); !slices.Equal(got, wantHeld) {
		t.Fatalf("held after a receipt = %q, want %q", got, wantHeld)
	}
	ctx := context.Background()
	now = now.Add(4 * time.Minute) // under 5 minutes after the message
	a.reconcile(ctx)
	if len(turns.sent) != 2 {
		t.Fatalf("before the user is quiet: sent %q", turns.sent)
	}
	now = now.Add(time.Minute)
	a.reconcile(ctx)
	want := []string{
		"<naozhi-watch>\n" + watchLine(watchKeyA, "第二轮") + "\n" + watchLine(keyC, "文档写完了") + "\n</naozhi-watch>",
		"<naozhi-watch>\n" + watchLine(watchKeyB, "查了网页") + "\n" + watchLine(keyD, "读了邮件") + "\n</naozhi-watch>",
	}
	if !slices.Equal(turns.sent[2:], want) || !slices.Equal(turns.srcs[2:], []Source{SourceWatch, SourceWatchOutside}) {
		t.Fatalf("quiet and idle: sent %q from %q, want %q", turns.sent[2:], turns.srcs[2:], want)
	}
	a.reconcile(ctx)
	if len(turns.sent) != 4 || len(heldWatch(a)) != 0 {
		t.Errorf("sent again %q, held %q", turns.sent[4:], heldWatch(a))
	}
}

// TestTellWatchWhileBusy: a watch line that comes while a Planner turn runs
// waits for it, in agent.json, so a restart keeps it; the reconcile of an
// idle Planner sends it.
func TestTellWatchWhileBusy(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, sess, turns := heldAgent(t, &now)
	sess.mu.Lock()
	sess.st.Running = true
	sess.mu.Unlock()
	if err := a.TellWatch(watchKeyA, watchLine(watchKeyA, "改好了"), false); err != nil {
		t.Fatal(err)
	}
	a.reconcile(context.Background())
	if len(turns.sent) != 0 || len(heldWatch(a)) != 1 {
		t.Fatalf("with a turn running: sent %q, held %q", turns.sent, heldWatch(a))
	}
	b := New(a.cfg)
	if got := heldWatch(b); len(got) != 1 {
		t.Fatalf("held after a restart: %q", got)
	}
	b.turns = turns
	b.markShift(now)
	b.reconcile(context.Background())
	if len(turns.sent) != 0 {
		t.Fatalf("the restarted run sent %q into a running turn", turns.sent)
	}
	sess.mu.Lock()
	sess.st.Running = false
	sess.mu.Unlock()
	b.reconcile(context.Background())
	if len(turns.sent) != 1 || turns.srcs[0] != SourceWatch || !strings.Contains(turns.sent[0], watchLine(watchKeyA, "改好了")) {
		t.Fatalf("idle: sent %q from %q", turns.sent, turns.srcs)
	}
	if c := New(a.cfg); len(heldWatch(c)) != 0 {
		t.Errorf("held once sent: %q", heldWatch(c))
	}
}

// TestTellWatchPlannerOff: a Planner stopped, not enabled or not started
// takes no watch line and keeps none; stopping drops the ones waiting (a
// turn end told once it is back on would be stale) and leaves the receipts.
func TestTellWatchPlannerOff(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.TellLater("已派出：乙"); err != nil {
		t.Fatal(err)
	}
	if err := a.TellWatch(watchKeyA, watchLine(watchKeyA, "改好了"), false); err != nil {
		t.Fatal(err)
	}
	if err := a.SetStopped(true); err != nil {
		t.Fatal(err)
	}
	if len(heldWatch(a)) != 0 || len(a.state.Held) != 1 || a.state.Held[0].Text != "已派出：乙" {
		t.Fatalf("held after the stop: %+v, want the receipt alone", a.state.Held)
	}
	if err := a.TellWatch(watchKeyA, watchLine(watchKeyA, "又一轮"), false); err != ErrStopped {
		t.Errorf("TellWatch on a stopped Planner = %v, want ErrStopped", err)
	}
	if len(heldWatch(a)) != 0 || len(turns.sent) != 1 {
		t.Errorf("stopped: held %q, sent %q", heldWatch(a), turns.sent)
	}
	if c := New(a.cfg); len(heldWatch(c)) != 0 {
		t.Errorf("agent.json keeps watch lines after the stop: %q", heldWatch(c))
	}

	off := New(Config{Layout: a.cfg.Layout, Sessions: a.cfg.Sessions, Now: a.cfg.Now})
	if err := off.TellWatch(watchKeyA, watchLine(watchKeyA, "x"), false); err != ErrOff {
		t.Errorf("TellWatch with the Planner not enabled = %v, want ErrOff", err)
	}
	fresh, _ := newTestAgent(t, &fakeSessions{}, &now)
	if err := fresh.TellWatch(watchKeyA, watchLine(watchKeyA, "x"), false); err != ErrNotReady {
		t.Errorf("TellWatch before Start = %v, want ErrNotReady", err)
	}
	if len(heldWatch(off))+len(heldWatch(fresh)) != 0 {
		t.Errorf("a Planner that takes none kept a line")
	}
}

// TestTellWatchOncePerWindow (D138): one session's turn ends wake the Planner
// once in 10 minutes. Those ending inside the window of the one told wait,
// a later one replacing the earlier, and go as one, the latest, when the
// window ends; the next window runs from that turn. Another session has a
// window of its own.
func TestTellWatchOncePerWindow(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	ctx := context.Background()
	tell := func(d time.Duration, key, last string) {
		t.Helper()
		now = now.Add(d)
		if err := a.TellWatch(key, watchLine(key, last), false); err != nil {
			t.Fatal(err)
		}
	}
	block := func(key, last string) string { return "<naozhi-watch>\n" + watchLine(key, last) + "\n</naozhi-watch>" }
	tell(0, watchKeyA, "第一轮")
	if !slices.Equal(turns.sent, []string{block(watchKeyA, "第一轮")}) {
		t.Fatalf("first turn: sent %q", turns.sent)
	}
	tell(3*time.Minute, watchKeyA, "第二轮")
	tell(3*time.Minute, watchKeyA, "第三轮")
	if len(turns.sent) != 1 || !slices.Equal(heldWatch(a), []string{watchKeyA + "=" + watchLine(watchKeyA, "第三轮")}) {
		t.Fatalf("inside the window: sent %q, held %q; want the latest waiting", turns.sent[1:], heldWatch(a))
	}
	tell(time.Minute, watchKeyB, "别的会话")
	if len(turns.sent) != 2 || turns.sent[1] != block(watchKeyB, "别的会话") {
		t.Fatalf("another session inside the first one's window: sent %q", turns.sent[1:])
	}
	now = now.Add(3*time.Minute - time.Second) // a second short of 10 minutes after the first
	a.reconcile(ctx)
	if len(turns.sent) != 2 {
		t.Fatalf("before the window's end: sent %q", turns.sent[2:])
	}
	now = now.Add(time.Second)
	a.reconcile(ctx)
	if len(turns.sent) != 3 || turns.sent[2] != block(watchKeyA, "第三轮") || len(heldWatch(a)) != 0 {
		t.Fatalf("at the window's end: sent %q, held %q; want the third turn once", turns.sent[2:], heldWatch(a))
	}
	// The window now runs from 14:10.
	tell(9*time.Minute, watchKeyA, "第四轮")
	a.reconcile(ctx)
	if len(turns.sent) != 3 {
		t.Fatalf("9 minutes after the merged one: sent %q", turns.sent[3:])
	}
	now = now.Add(time.Minute)
	a.reconcile(ctx)
	if len(turns.sent) != 4 || turns.sent[3] != block(watchKeyA, "第四轮") {
		t.Fatalf("second window's end: sent %q", turns.sent[3:])
	}
	// Past its window a turn end goes at once again.
	tell(11*time.Minute, watchKeyA, "第五轮")
	if len(turns.sent) != 5 || len(heldWatch(a)) != 0 {
		t.Fatalf("past the window: sent %q, held %q", turns.sent[4:], heldWatch(a))
	}
}

// TestTellWatchWindowFromTheTurnSent: the 10 minutes run from the turn a
// session's line went in, not from when it was handed in. One that waited
// 9m50s on a busy Planner is followed by the next no sooner than 10 minutes
// after it went; meanwhile a later turn end replaces the one waiting, so
// the line that goes is the latest.
func TestTellWatchWindowFromTheTurnSent(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, sess, turns := heldAgent(t, &now)
	ctx := context.Background()
	busy := func(on bool) {
		sess.mu.Lock()
		sess.st.Running = on
		sess.mu.Unlock()
	}
	tell := func(last string) {
		t.Helper()
		if err := a.TellWatch(watchKeyA, watchLine(watchKeyA, last), false); err != nil {
			t.Fatal(err)
		}
	}
	busy(true)
	tell("第一轮")
	now = now.Add(5 * time.Minute)
	tell("第二轮") // the Planner still busy: nothing went, so no window holds it back
	now = now.Add(4*time.Minute + 50*time.Second)
	busy(false)
	a.reconcile(ctx)
	if len(turns.sent) != 1 || !strings.Contains(turns.sent[0], watchLine(watchKeyA, "第二轮")) || strings.Contains(turns.sent[0], "第一轮") {
		t.Fatalf("idle at 14:09:50: sent %q, want the latest turn alone", turns.sent)
	}
	now = now.Add(20 * time.Second)
	tell("第三轮")
	if len(turns.sent) != 1 {
		t.Fatalf("20 seconds after the last one went: sent %q", turns.sent[1:])
	}
	now = now.Add(9*time.Minute + 39*time.Second)
	a.reconcile(ctx)
	if len(turns.sent) != 1 {
		t.Fatalf("a second short of 10 minutes after it went: sent %q", turns.sent[1:])
	}
	now = now.Add(time.Second)
	a.reconcile(ctx)
	if len(turns.sent) != 2 || !strings.Contains(turns.sent[1], watchLine(watchKeyA, "第三轮")) {
		t.Fatalf("10 minutes after it went: sent %q", turns.sent[1:])
	}
}

// TestTellWatchCheckedAsItGoes: a line that waited goes as its session
// stands when it goes, not as it was handed in (SetWatchNow). One whose
// session read outside content meanwhile goes as an outside turn's, which
// may not write the ledger; one whose 观察 was turned off, or whose session
// is gone, does not go at all; a line handed in as outside stays so.
func TestTellWatchCheckedAsItGoes(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	a, _, turns := heldAgent(t, &now)
	const keyC, keyD = "dashboard:direct:u:docs", "dashboard:direct:u:mail"
	gone, tainted := map[string]bool{}, map[string]bool{}
	a.SetWatchNow(func(key, line string) (string, bool, bool) {
		if gone[key] {
			return "", false, false
		}
		return line + "@now", tainted[key], true
	})
	if err := a.Send("在吗", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		key     string
		outside bool
	}{{watchKeyA, false}, {watchKeyB, false}, {keyC, false}, {keyD, true}} {
		if err := a.TellWatch(w.key, watchLine(w.key, "改好了"), w.outside); err != nil {
			t.Fatal(err)
		}
	}
	if len(turns.sent) != 1 || len(heldWatch(a)) != 4 {
		t.Fatalf("while chatting: sent %q, held %q", turns.sent[1:], heldWatch(a))
	}
	// Meanwhile: A's 观察 is turned off, B's next turn fetches a web page.
	gone[watchKeyA], tainted[watchKeyB] = true, true
	now = now.Add(5 * time.Minute)
	a.reconcile(context.Background())
	want := []string{
		"<naozhi-watch>\n" + watchLine(keyC, "改好了") + "@now\n</naozhi-watch>",
		"<naozhi-watch>\n" + watchLine(watchKeyB, "改好了") + "@now\n" + watchLine(keyD, "改好了") + "@now\n</naozhi-watch>",
	}
	if !slices.Equal(turns.sent[1:], want) || !slices.Equal(turns.srcs[1:], []Source{SourceWatch, SourceWatchOutside}) {
		t.Fatalf("quiet: sent %q from %q, want %q from watch, watch_outside", turns.sent[1:], turns.srcs[1:], want)
	}
	if got := heldWatch(a); len(got) != 0 {
		t.Errorf("held after: %q, want the dropped line gone too", got)
	}
	if c := New(a.cfg); len(heldWatch(c)) != 0 {
		t.Errorf("agent.json keeps %q", heldWatch(c))
	}
}
