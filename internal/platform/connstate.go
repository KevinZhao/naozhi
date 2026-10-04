package platform

import (
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// ConnStateKind is where a platform's link to its IM service stands.
type ConnStateKind string

const (
	ConnConnecting ConnStateKind = "connecting"
	ConnConnected  ConnStateKind = "connected"
	// ConnDisconnected: the link dropped and the adapter is retrying.
	ConnDisconnected ConnStateKind = "disconnected"
	// ConnFailed: the adapter gave up; only a restart (or a config fix) recovers.
	ConnFailed ConnStateKind = "failed"
)

// connErrorMax caps LastError. The text comes from SDK errors and is served on
// the authenticated /health section, so it is sanitised as well as bounded.
const connErrorMax = 256

// ConnState is one platform's connection state. Since is when State last
// changed; LastError / LastErrorAt are the most recent failure and survive a
// later recovery, so "connected, last error 3m ago" stays visible.
type ConnState struct {
	State       ConnStateKind
	Since       time.Time
	LastError   string
	LastErrorAt time.Time
}

// ConnStateReporter is an optional capability for platforms that can observe
// their own connection. ok=false means "not observable right now" (feishu in
// webhook mode has no connection to watch, an adapter that has not started has
// nothing to report), and callers fall back to "registered".
type ConnStateReporter interface {
	ConnState() (state ConnState, ok bool)
}

// ConnStateOf asks p for its connection state; ok=false when p is nil, does
// not implement ConnStateReporter, or declines to answer.
func ConnStateOf(p Platform) (ConnState, bool) {
	if p == nil {
		return ConnState{}, false
	}
	r, ok := AsCapability[ConnStateReporter](p)
	if !ok {
		return ConnState{}, false
	}
	return r.ConnState()
}

// ConnStatesOf reports the observable connection state of every named
// platform, keyed by registry name. Platforms that cannot answer are left out,
// and the result is nil when none can.
func ConnStatesOf(platforms map[string]Platform) map[string]ConnState {
	var out map[string]ConnState
	for name, p := range platforms {
		s, ok := ConnStateOf(p)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]ConnState, len(platforms))
		}
		out[name] = s
	}
	return out
}

// ConnTracker is the state an adapter feeds from its SDK's lifecycle hooks.
// The zero value is ready to use and reports nothing until the first Set or
// Fail. Every method takes one short mutex section and never blocks, so it is
// safe to call from an SDK's read loop.
type ConnTracker struct {
	mu  sync.Mutex
	s   ConnState
	now func() time.Time // nil means time.Now; tests pin it
}

func (t *ConnTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// setLocked moves to kind, stamping Since only on an actual change so a
// repeated "connected" (a reconnect hook firing twice) keeps the original time.
func (t *ConnTracker) setLocked(kind ConnStateKind, now time.Time) {
	if t.s.State == kind {
		return
	}
	t.s.State = kind
	t.s.Since = now
}

// Set records a transition to kind.
func (t *ConnTracker) Set(kind ConnStateKind) {
	t.mu.Lock()
	t.setLocked(kind, t.clock())
	t.mu.Unlock()
}

// Fail records a transition to kind together with the error that caused it.
// A nil err is a plain Set.
func (t *ConnTracker) Fail(kind ConnStateKind, err error) {
	t.mu.Lock()
	now := t.clock()
	t.setLocked(kind, now)
	if err != nil {
		t.s.LastError = osutil.SanitizeForLog(err.Error(), connErrorMax)
		t.s.LastErrorAt = now
	}
	t.mu.Unlock()
}

// NoteError records err without changing the state, for SDK hooks that report
// an error and leave reconnecting to a later hook. A nil err is ignored.
func (t *ConnTracker) NoteError(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.s.LastError = osutil.SanitizeForLog(err.Error(), connErrorMax)
	t.s.LastErrorAt = t.clock()
	t.mu.Unlock()
}

// Snapshot returns the current state; ok=false until the first Set or Fail.
// NoteError alone does not make the state observable.
func (t *ConnTracker) Snapshot() (ConnState, bool) {
	t.mu.Lock()
	s := t.s
	t.mu.Unlock()
	return s, s.State != ""
}
