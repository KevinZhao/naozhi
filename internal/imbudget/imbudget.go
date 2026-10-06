// Package imbudget is the per-chat spend gate for IM messages: it answers
// "may this chat start another turn" from a rolling-window USD total the
// caller reads out of the cost ledger. Leaf package (stdlib only); the ledger
// read is injected as SpendFunc. See docs/rfc/im-usage-limits.md.
package imbudget

import (
	"errors"
	"sync"
	"time"
)

// Action is what the gate does once a chat reaches its limit.
type Action string

const (
	// ActionBlock refuses new turns at the limit.
	ActionBlock Action = "block"
	// ActionWarn only reports; Decision.Block is never set.
	ActionWarn Action = "warn"
)

const (
	// DefaultWarnRatio is the share of the limit at which Decision.Warn fires.
	DefaultWarnRatio = 0.8
	// DefaultWindow is the rolling window the limit applies to.
	DefaultWindow = 24 * time.Hour
	// DefaultRefresh is how long a chat's cached total is reused before the
	// ledger is read again. A turn's cost lands only after it ends, so the
	// gate is already after-the-fact by one turn; this adds at most one
	// refresh of staleness.
	DefaultRefresh = 15 * time.Second
	// maxChats bounds the cache; past it the stalest chat is evicted.
	maxChats = 1024
)

// ErrPolicy is returned by Policy.Validate for an out-of-range field.
var ErrPolicy = errors.New("imbudget: invalid policy")

// Policy is the operator's limit. The zero value disables the gate.
type Policy struct {
	// PerChatUSD is the USD limit per chat per Window; 0 disables the gate.
	PerChatUSD float64
	// WarnRatio in (0,1) is where the one-per-window warning fires; 0 means
	// DefaultWarnRatio.
	WarnRatio float64
	// Action defaults to ActionBlock.
	Action Action
	// Window defaults to DefaultWindow.
	Window time.Duration
}

// Enabled reports whether the policy limits anything.
func (p Policy) Enabled() bool { return p.PerChatUSD > 0 }

// Validate rejects a policy the gate could not honour as written.
func (p Policy) Validate() error {
	switch {
	case p.PerChatUSD < 0:
		return errors.New("imbudget: per-chat limit is negative")
	case p.WarnRatio < 0 || p.WarnRatio >= 1:
		return errors.New("imbudget: warn ratio must be in [0,1)")
	case p.Action != "" && p.Action != ActionBlock && p.Action != ActionWarn:
		return errors.New("imbudget: action must be block or warn")
	case p.Window < 0:
		return errors.New("imbudget: window is negative")
	}
	return nil
}

func (p Policy) normalized() Policy {
	if p.WarnRatio == 0 {
		p.WarnRatio = DefaultWarnRatio
	}
	if p.Action == "" {
		p.Action = ActionBlock
	}
	if p.Window == 0 {
		p.Window = DefaultWindow
	}
	return p
}

// SpendFunc returns chatKey's USD spend in [from, to). An error makes the
// gate fail open for that check and is counted (see Gate.LedgerErrors).
type SpendFunc func(chatKey string, from, to time.Time) (float64, error)

// Decision is one Check's answer.
type Decision struct {
	// Spent and Limit are the figures a reply quotes.
	Spent, Limit float64
	// Block is set when Action is block and Spent >= Limit.
	Block bool
	// Warn is set the first time in a window Spent crosses WarnRatio*Limit.
	Warn bool
	// LedgerError is set when the spend read failed and the gate let the
	// message through without a figure.
	LedgerError bool
}

type chatState struct {
	spent  float64
	asOf   time.Time
	warned time.Time // last warn fired; zero = never
}

// Gate applies one Policy across chats. Safe for concurrent use.
type Gate struct {
	pol     Policy
	spend   SpendFunc
	refresh time.Duration
	now     func() time.Time

	mu     sync.Mutex
	chats  map[string]*chatState
	errors uint64
}

// Option tunes a Gate.
type Option func(*Gate)

// WithRefresh sets how long a cached total is reused; 0 reads the ledger on
// every Check.
func WithRefresh(d time.Duration) Option { return func(g *Gate) { g.refresh = d } }

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(g *Gate) { g.now = now } }

// New builds a gate; a disabled policy yields a gate whose Check always
// passes. spend must be non-nil when the policy is enabled.
func New(p Policy, spend SpendFunc, opts ...Option) *Gate {
	g := &Gate{pol: p.normalized(), spend: spend, refresh: DefaultRefresh, now: time.Now, chats: make(map[string]*chatState)}
	for _, o := range opts {
		o(g)
	}
	if g.pol.Enabled() && g.spend == nil {
		panic("imbudget: enabled policy needs a SpendFunc")
	}
	return g
}

// Policy returns the normalized policy in force.
func (g *Gate) Policy() Policy { return g.pol }

// LedgerErrors counts Checks that failed open because the spend read failed.
func (g *Gate) LedgerErrors() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.errors
}

// Check decides whether chatKey may start a turn now. A nil or disabled
// gate always passes with a zero Decision.
func (g *Gate) Check(chatKey string) Decision {
	if g == nil || !g.pol.Enabled() {
		return Decision{}
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.chats[chatKey]
	if st == nil {
		g.evictIfFull(now)
		st = &chatState{}
		g.chats[chatKey] = st
	}
	d := Decision{Limit: g.pol.PerChatUSD}
	if st.asOf.IsZero() || g.refresh == 0 || now.Sub(st.asOf) >= g.refresh {
		spent, err := g.spend(chatKey, now.Add(-g.pol.Window), now)
		if err != nil {
			g.errors++
			if st.asOf.IsZero() {
				d.LedgerError = true
				return d
			}
			// Keep the last good figure rather than failing open outright.
		} else {
			st.spent, st.asOf = spent, now
		}
	}
	d.Spent = st.spent
	if d.Spent >= g.pol.WarnRatio*g.pol.PerChatUSD && now.Sub(st.warned) >= g.pol.Window {
		st.warned = now
		d.Warn = true
	}
	d.Block = g.pol.Action == ActionBlock && d.Spent >= g.pol.PerChatUSD
	return d
}

// evictIfFull drops the chat with the oldest asOf when the cache is at
// maxChats; called with mu held.
func (g *Gate) evictIfFull(now time.Time) {
	if len(g.chats) < maxChats {
		return
	}
	var victim string
	oldest := now
	for k, st := range g.chats {
		if victim == "" || st.asOf.Before(oldest) {
			victim, oldest = k, st.asOf
		}
	}
	delete(g.chats, victim)
}
