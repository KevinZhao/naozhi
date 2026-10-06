package imbudget

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeLedger struct {
	spent map[string]float64
	calls int
	err   error
}

func (f *fakeLedger) fn(chat string, _, _ time.Time) (float64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return f.spent[chat], nil
}

func newGate(t *testing.T, p Policy, l *fakeLedger, now *time.Time, opts ...Option) *Gate {
	t.Helper()
	opts = append(opts, WithClock(func() time.Time { return *now }))
	return New(p, l.fn, opts...)
}

func TestCheck_DisabledOrNilAlwaysPasses(t *testing.T) {
	t.Parallel()
	var g *Gate
	if d := g.Check("x"); d.Block || d.Warn {
		t.Fatalf("nil gate decided %+v", d)
	}
	g = New(Policy{}, nil)
	if d := g.Check("x"); d != (Decision{}) {
		t.Fatalf("disabled gate decided %+v", d)
	}
}

func TestCheck_BlockWarnOnceAndRecover(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	l := &fakeLedger{spent: map[string]float64{"c": 1}}
	g := newGate(t, Policy{PerChatUSD: 10}, l, &now, WithRefresh(0))

	if d := g.Check("c"); d.Block || d.Warn || d.Spent != 1 || d.Limit != 10 {
		t.Fatalf("under limit: %+v", d)
	}
	l.spent["c"] = 8 // crosses the default 0.8 warn ratio
	d := g.Check("c")
	if !d.Warn || d.Block {
		t.Fatalf("at warn ratio: %+v", d)
	}
	if d := g.Check("c"); d.Warn {
		t.Fatalf("warn repeated within the window: %+v", d)
	}
	l.spent["c"] = 10
	if d := g.Check("c"); !d.Block || d.Spent != 10 {
		t.Fatalf("at limit: %+v", d)
	}
	now = now.Add(DefaultWindow)
	l.spent["c"] = 9
	if d := g.Check("c"); !d.Warn || d.Block {
		t.Fatalf("new window re-arms the warning: %+v", d)
	}
}

func TestCheck_WarnActionNeverBlocks(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := &fakeLedger{spent: map[string]float64{"c": 99}}
	g := newGate(t, Policy{PerChatUSD: 10, Action: ActionWarn}, l, &now)
	if d := g.Check("c"); d.Block || !d.Warn {
		t.Fatalf("warn action: %+v", d)
	}
}

func TestCheck_RefreshCachesPerChat(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := &fakeLedger{spent: map[string]float64{"c": 1}}
	g := newGate(t, Policy{PerChatUSD: 10}, l, &now, WithRefresh(15*time.Second))
	g.Check("c")
	l.spent["c"] = 50
	if d := g.Check("c"); d.Block || l.calls != 1 {
		t.Fatalf("cached figure should be reused: %+v calls=%d", d, l.calls)
	}
	now = now.Add(15 * time.Second)
	if d := g.Check("c"); !d.Block || l.calls != 2 {
		t.Fatalf("stale cache should re-read: %+v calls=%d", d, l.calls)
	}
	g.Check("other")
	if l.calls != 3 {
		t.Fatalf("another chat is its own cache entry, calls=%d", l.calls)
	}
}

func TestCheck_LedgerErrorFailsOpenThenKeepsLastGood(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := &fakeLedger{err: errors.New("disk")}
	g := newGate(t, Policy{PerChatUSD: 10}, l, &now, WithRefresh(0))
	d := g.Check("c")
	if d.Block || !d.LedgerError || g.LedgerErrors() != 1 {
		t.Fatalf("first read failing must fail open and count: %+v errs=%d", d, g.LedgerErrors())
	}
	l.err, l.spent = nil, map[string]float64{"c": 12}
	if d := g.Check("c"); !d.Block {
		t.Fatalf("recovered read: %+v", d)
	}
	l.err = errors.New("disk again")
	if d := g.Check("c"); !d.Block || d.LedgerError || d.Spent != 12 {
		t.Fatalf("a later failure keeps the last good figure: %+v", d)
	}
}

func TestCheck_CacheBounded(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := &fakeLedger{spent: map[string]float64{}}
	g := newGate(t, Policy{PerChatUSD: 10}, l, &now)
	for i := 0; i < maxChats+50; i++ {
		g.Check(fmt.Sprintf("chat-%d", i))
		now = now.Add(time.Millisecond)
	}
	g.mu.Lock()
	n := len(g.chats)
	_, oldestKept := g.chats["chat-0"]
	g.mu.Unlock()
	if n != maxChats || oldestKept {
		t.Fatalf("cache size %d (want %d), chat-0 kept=%v", n, maxChats, oldestKept)
	}
}

func TestPolicy_Validate(t *testing.T) {
	t.Parallel()
	bad := []Policy{
		{PerChatUSD: -1},
		{PerChatUSD: 1, WarnRatio: 1},
		{PerChatUSD: 1, WarnRatio: -0.1},
		{PerChatUSD: 1, Action: "maybe"},
		{PerChatUSD: 1, Window: -time.Hour},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("bad[%d] %+v accepted", i, p)
		}
	}
	good := Policy{PerChatUSD: 5}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	n := good.normalized()
	if n.WarnRatio != DefaultWarnRatio || n.Action != ActionBlock || n.Window != DefaultWindow {
		t.Fatalf("normalized = %+v", n)
	}
}

func TestNew_EnabledWithoutSpendPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	New(Policy{PerChatUSD: 1}, nil)
}
