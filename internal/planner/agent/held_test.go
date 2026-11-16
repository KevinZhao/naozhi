package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
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
