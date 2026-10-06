package dispatch

import (
	"context"
	"expvar"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

const budgetSpentReply = "今日费用预算已用尽"

// budgetNow is noon UTC, so the gate's day (UTC) cannot turn mid-test.
var budgetNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// newBudgetDispatcher wires a gate with lim over an index the test books
// spend into with spend(sessionKey, usd).
func newBudgetDispatcher(t *testing.T, lim budget.Limits) (*Dispatcher, *fakePlatform, *countingTurns, func(string, float64)) {
	t.Helper()
	idx := budget.NewIndex(time.UTC, func() time.Time { return budgetNow })
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, func(cfg *testDispatcherConfig) { cfg.Budget = budget.NewGate(lim, idx) })
	ct := &countingTurns{Turns: d.turns}
	d.turns = ct
	spend := func(key string, usd float64) {
		idx.Add(costledger.Entry{TS: budgetNow, SessionKey: key, Unit: costledger.UnitUSD, Amount: usd})
	}
	return d, fp, ct, spend
}

func budgetMsg(chatID, text string) platform.IncomingMessage {
	return rateMsg("alice", chatID, text)
}

// blockedCount reads one scope of naozhi_dispatch_budget_blocked_total.
func blockedCount(scope string) int64 {
	if v, ok := dispatchBudgetBlockedTotal.Get(scope).(*expvar.Int); ok {
		return v.Value()
	}
	return 0
}

// A chat at its cap gets no turn, for a message or /urgent, and is told once
// per window; commands still work there, and another chat is unaffected.
func TestBudget_ChatAtCapIsRefused(t *testing.T) {
	d, fp, ct, spend := newBudgetDispatcher(t, budget.Limits{PerChatDailyUSD: 1})
	spend("fake:direct:c1:general", 0.6)
	spend("fake:direct:c1:reviewer", 0.4) // the same chat through another agent
	before := blockedCount("chat")
	h := d.BuildHandler()

	h(context.Background(), budgetMsg("c1", "hello"))
	h(context.Background(), budgetMsg("c1", "again"))
	h(context.Background(), budgetMsg("c1", "/urgent now"))
	if n := ct.submits.Load(); n != 0 {
		t.Fatalf("submits in a spent chat = %d, want 0", n)
	}
	if n := countReplies(fp, budgetSpentReply); n != 1 {
		t.Errorf("budget replies = %d, want 1: %q", n, fp.allReplies())
	}
	want := "今日费用预算已用尽（本会话 $1.00 / $1.00），09-07 00:00 重置；在此之前新消息不会处理。"
	if got := fp.allReplies(); len(got) == 0 || got[0] != want {
		t.Errorf("first reply = %q, want %q", got, want)
	}
	if moved := blockedCount("chat") - before; moved != 3 {
		t.Errorf("naozhi_dispatch_budget_blocked_total[chat] moved by %d, want 3", moved)
	}

	h(context.Background(), budgetMsg("c1", "/new"))
	if n := ct.resets.Load(); n != 1 {
		t.Errorf("/new in a spent chat reset %d sessions, want 1", n)
	}
	h(context.Background(), budgetMsg("c2", "hello"))
	if n := ct.submits.Load(); n != 1 {
		t.Errorf("submits from another chat = %d, want 1", n)
	}
}

// The machine-wide cap refuses every chat, and each chat is told.
func TestBudget_GlobalCapRefusesEveryChat(t *testing.T) {
	d, fp, ct, spend := newBudgetDispatcher(t, budget.Limits{PerChatDailyUSD: 50, DailyUSD: 10})
	spend("dashboard:direct:x:general", 10) // dashboard spend counts toward the total
	h := d.BuildHandler()
	h(context.Background(), budgetMsg("c1", "hello"))
	h(context.Background(), budgetMsg("c2", "hello"))
	if n := ct.submits.Load(); n != 0 {
		t.Fatalf("submits past the global cap = %d, want 0", n)
	}
	if n := countReplies(fp, "（本机 $10.00 / $10.00）"); n != 2 {
		t.Errorf("global budget replies = %d, want one per chat: %q", n, fp.allReplies())
	}
}

// Under action warn nothing is refused.
func TestBudget_WarnActionNeverRefuses(t *testing.T) {
	d, _, ct, spend := newBudgetDispatcher(t, budget.Limits{PerChatDailyUSD: 1, Action: budget.ActionWarn})
	spend("fake:direct:c1:general", 5)
	d.BuildHandler()(context.Background(), budgetMsg("c1", "hello"))
	if n := ct.submits.Load(); n != 1 {
		t.Errorf("submits under action warn = %d, want 1", n)
	}
}

// A typed nil gate in the config admits everything instead of panicking.
func TestBudget_NilGateAdmits(t *testing.T) {
	fp := &fakePlatform{}
	var g *budget.Gate
	d := newTestDispatcher(fp, func(cfg *testDispatcherConfig) { cfg.Budget = g })
	if d.budget != nil {
		t.Fatal("a nil *budget.Gate must leave the dispatcher ungated")
	}
	if line := d.budgetWarnLine("fake:direct:c1:general"); line != "" {
		t.Errorf("warn line without a gate = %q", line)
	}
}

// replyWithBudget delivers one answer on key and returns what the chat got.
func replyWithBudget(t *testing.T, d *Dispatcher, fp *fakePlatform, key string) string {
	t.Helper()
	return replyTextWithBudget(t, d, fp, key, "answer")
}

// replyTextWithBudget delivers an answer of text on key and returns the
// chat's last reply.
func replyTextWithBudget(t *testing.T, d *Dispatcher, fp *fakePlatform, key, text string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := budgetMsg("c1", "hello")
	o := d.newIMOrigin(msg, slog.Default(), key, "general", session.AgentOpts{}, imMessage, len(msg.Text), 0)
	dl := &imDelivery{o: o, p: fp, lg: slog.Default()}
	dl.BeforeSession(ctx)
	dl.reply(ctx, &clievent.SendResult{Text: text}, nil)
	dl.tracker.stop()
	return fp.lastReply()
}

// A reply ends with one warning line the first time each day its chat
// passes warn_ratio, and again when it passes the cap; a planner key warns
// for its project. An empty answer sends nothing and leaves the mark unused.
func TestBudget_ReplyWarnsOncePerLevel(t *testing.T) {
	d, fp, _, spend := newBudgetDispatcher(t, budget.Limits{PerChatDailyUSD: 10})
	key := "fake:direct:c1:general"
	spend(key, 8)
	replyTextWithBudget(t, d, fp, key, "")
	if n := fp.replyCount(); n != 0 {
		t.Fatalf("an empty answer sent %d replies: %q", n, fp.allReplies())
	}
	if got := replyWithBudget(t, d, fp, key); got != "answer\n\n⚠️ 今日费用已达预算的 80%（本会话 $8.00 / $10.00）" {
		t.Errorf("first reply at 80%% = %q", got)
	}
	if got := replyWithBudget(t, d, fp, key); got != "answer" {
		t.Errorf("second reply at 80%% = %q, want no warning", got)
	}
	spend(key, 2)
	want := "answer\n\n⚠️ 今日费用预算已用尽（本会话 $10.00 / $10.00），09-07 00:00 前新消息不会处理"
	if got := replyWithBudget(t, d, fp, key); got != want {
		t.Errorf("reply at the cap = %q, want %q", got, want)
	}

	spend("project:naozhi:planner", 9)
	if got := replyWithBudget(t, d, fp, "project:naozhi:planner"); !strings.HasSuffix(got, "（项目 naozhi $9.00 / $10.00）") {
		t.Errorf("planner reply = %q, want the project's warning", got)
	}
}

// Under action warn a reply past the cap says it is over, without refusing.
func TestBudget_ReplyWarnActionOver(t *testing.T) {
	d, fp, _, spend := newBudgetDispatcher(t, budget.Limits{DailyUSD: 10, Action: budget.ActionWarn})
	spend("fake:direct:c9:general", 12)
	if got := replyWithBudget(t, d, fp, "fake:direct:c1:general"); got != "answer\n\n⚠️ 今日费用已超出预算（本机 $12.00 / $10.00）" {
		t.Errorf("reply over the global cap = %q", got)
	}
}
