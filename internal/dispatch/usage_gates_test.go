package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/imbudget"
	"github.com/naozhi/naozhi/internal/ratelimit"
)

// spendTable is a SpendFunc backed by a map the test mutates between checks.
type spendTable map[string]float64

func (s spendTable) fn(chat string, _, _ time.Time) (float64, error) { return s[chat], nil }

func newGatedDispatcher(t *testing.T, g *imbudget.Gate, l *ratelimit.Limiter) (*Dispatcher, *fakePlatform, *countingTurns) {
	t.Helper()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, func(cfg *testDispatcherConfig) {
		cfg.Budget = g
		cfg.UserRate = l
	})
	ct := &countingTurns{Turns: d.turns}
	d.turns = ct
	return d, fp, ct
}

func TestBudgetGate_BlocksTurnsButNotCommands(t *testing.T) {
	spend := spendTable{}
	g := imbudget.New(imbudget.Policy{PerChatUSD: 10}, spend.fn, imbudget.WithRefresh(0))
	d, fp, ct := newGatedDispatcher(t, g, nil)
	ctx := context.Background()
	alice := func(text string) bool {
		_, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", text))
		return ok
	}
	before := d.messageCount.Load()

	if !alice("hello") {
		t.Fatal("under budget, plain message refused")
	}
	spend["fake:direct:chat-alice"] = 10
	if alice("hello again") {
		t.Fatal("at budget, plain message accepted")
	}
	if got := fp.lastReply(); !strings.Contains(got, "预算已用尽") || !strings.Contains(got, "$10.00 / $10.00") {
		t.Fatalf("refusal = %q", got)
	}
	// The refusal is throttled per chat.
	n := fp.replyCount()
	alice("third")
	if fp.replyCount() != n {
		t.Fatal("second refusal inside the window replied again")
	}
	// Commands still run: /help answers, /new resets.
	alice("/help")
	if got := fp.lastReply(); !strings.Contains(got, "可用命令") {
		t.Fatalf("/help at budget = %q", got)
	}
	resets := ct.resets.Load()
	alice("/new")
	if ct.resets.Load() != resets+1 {
		t.Fatal("/new at budget did not reach Turns.Reset")
	}
	if d.messageCount.Load() != before+1 {
		t.Fatalf("accepted-message count moved by %d, want 1 (only the under-budget message)", d.messageCount.Load()-before)
	}
	// Another chat is unaffected.
	if _, ok := d.prepareInbound(ctx, authzMsg("bob", "direct", "hi")); !ok {
		t.Fatal("another chat was refused by alice's budget")
	}
}

func TestBudgetGate_WarnOnceThenWarnActionNeverBlocks(t *testing.T) {
	spend := spendTable{"fake:direct:chat-alice": 9}
	g := imbudget.New(imbudget.Policy{PerChatUSD: 10, Action: imbudget.ActionWarn}, spend.fn, imbudget.WithRefresh(0))
	d, fp, _ := newGatedDispatcher(t, g, nil)
	ctx := context.Background()

	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "hello")); !ok {
		t.Fatal("warn action refused a message")
	}
	if got := fp.lastReply(); !strings.Contains(got, "已使用 $9.00 / $10.00") {
		t.Fatalf("warning = %q", got)
	}
	n := fp.replyCount()
	spend["fake:direct:chat-alice"] = 50
	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "more")); !ok {
		t.Fatal("warn action blocked past the limit")
	}
	if fp.replyCount() != n {
		t.Fatal("warning repeated within the window")
	}
}

func TestRateLimit_DropsBeforeCommandsAndIsPerSender(t *testing.T) {
	l := ratelimit.New(ratelimit.Config{Rate: rate.Every(time.Hour), Burst: 2})
	d, fp, ct := newGatedDispatcher(t, nil, l)
	ctx := context.Background()

	d.prepareInbound(ctx, authzMsg("alice", "direct", "/help"))
	d.prepareInbound(ctx, authzMsg("alice", "direct", "/help"))
	helps := fp.replyCount()
	if helps != 2 {
		t.Fatalf("burst of 2 answered %d times", helps)
	}
	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "/help")); ok {
		t.Fatal("third message inside the bucket was accepted")
	}
	// One notice, then silence: /help itself must not have run.
	if got := fp.lastReply(); !strings.Contains(got, "发送过快") {
		t.Fatalf("rate-limit notice = %q", got)
	}
	d.prepareInbound(ctx, authzMsg("alice", "direct", "hello"))
	if fp.replyCount() != helps+1 || ct.submits.Load() != 0 {
		t.Fatalf("limited sender got replies=%d submits=%d, want one notice and no turn", fp.replyCount()-helps, ct.submits.Load())
	}
	// bob has his own bucket.
	if _, ok := d.prepareInbound(ctx, authzMsg("bob", "direct", "hi")); !ok {
		t.Fatal("another sender was limited by alice's bucket")
	}
}

func TestUsageGates_AbsentIsNoop(t *testing.T) {
	d, _, _ := newGatedDispatcher(t, nil, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "hello")); !ok {
			t.Fatalf("message %d refused with no gates configured", i)
		}
	}
	// Runtime swap-in takes effect on the next message.
	d.SetBudgetGate(imbudget.New(imbudget.Policy{PerChatUSD: 1}, spendTable{"fake:direct:chat-alice": 5}.fn, imbudget.WithRefresh(0)))
	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "hello")); ok {
		t.Fatal("swapped-in gate did not apply")
	}
	d.SetBudgetGate(nil)
	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "hello")); !ok {
		t.Fatal("clearing the gate did not reopen the chat")
	}
}
