package platform

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// connPlat satisfies ConnStateReporter with a canned answer.
type connPlat struct {
	fakePlat
	s  ConnState
	ok bool
}

func (c connPlat) ConnState() (ConnState, bool) { return c.s, c.ok }

// steppingClock returns a tracker clock that advances one second per read.
func steppingClock(start time.Time) func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return start.Add(time.Duration(n) * time.Second)
	}
}

func TestConnTracker_ZeroValueReportsNothing(t *testing.T) {
	var tr ConnTracker
	if s, ok := tr.Snapshot(); ok || s != (ConnState{}) {
		t.Fatalf("zero tracker Snapshot = %+v, %v; want zero, false", s, ok)
	}
	tr.NoteError(errors.New("early"))
	if _, ok := tr.Snapshot(); ok {
		t.Error("NoteError alone made the tracker observable; a state must be set first")
	}
	tr.NoteError(nil)
	tr.Set(ConnConnecting)
	s, ok := tr.Snapshot()
	if !ok || s.State != ConnConnecting || s.Since.IsZero() {
		t.Errorf("after Set: %+v, %v; want connecting with Since stamped", s, ok)
	}
	if s.LastError != "early" {
		t.Errorf("LastError = %q, want the earlier NoteError kept", s.LastError)
	}
}

func TestConnTracker_SinceMovesOnlyOnTransition(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := ConnTracker{now: steppingClock(t0)}

	tr.Set(ConnConnecting) // t0+1s
	first, _ := tr.Snapshot()
	tr.Set(ConnConnecting) // t0+2s, same state
	tr.Fail(ConnConnecting, errors.New("dial timeout"))
	again, _ := tr.Snapshot()
	if !again.Since.Equal(first.Since) {
		t.Errorf("repeating connecting moved Since %v -> %v", first.Since, again.Since)
	}
	if !again.LastErrorAt.After(first.Since) {
		t.Errorf("LastErrorAt = %v, want the Fail's own time (after %v)", again.LastErrorAt, first.Since)
	}

	tr.Set(ConnConnected)
	up, _ := tr.Snapshot()
	if up.State != ConnConnected || !up.Since.After(again.LastErrorAt) {
		t.Errorf("after connect: %+v; want connected with a fresh Since", up)
	}
	if up.LastError != "dial timeout" {
		t.Errorf("LastError = %q, want it to survive the recovery", up.LastError)
	}

	tr.NoteError(errors.New("ping lost"))
	noted, _ := tr.Snapshot()
	if noted.State != ConnConnected || !noted.Since.Equal(up.Since) || noted.LastError != "ping lost" {
		t.Errorf("NoteError: %+v; want state and Since unchanged, error replaced", noted)
	}
}

func TestConnTracker_FailSanitisesAndCapsError(t *testing.T) {
	var tr ConnTracker
	long := "bad\nline\x1b[31m" + strings.Repeat("x", 400)
	tr.Fail(ConnFailed, errors.New(long))
	s, ok := tr.Snapshot()
	if !ok || s.State != ConnFailed {
		t.Fatalf("Snapshot = %+v, %v; want failed", s, ok)
	}
	if len(s.LastError) != connErrorMax {
		t.Errorf("len(LastError) = %d, want capped at %d", len(s.LastError), connErrorMax)
	}
	if strings.ContainsAny(s.LastError, "\n\x1b") {
		t.Errorf("LastError = %q, want control bytes replaced", s.LastError)
	}
	if !strings.HasPrefix(s.LastError, "bad_line_[31m") {
		t.Errorf("LastError = %q, want the sanitised text kept", s.LastError[:20])
	}

	tr.Fail(ConnDisconnected, nil)
	s, _ = tr.Snapshot()
	if s.State != ConnDisconnected || !strings.HasPrefix(s.LastError, "bad") {
		t.Errorf("Fail(nil): %+v; want a plain transition keeping the last error", s)
	}
}

// TestConnTracker_FailIfOnlyFromState: a probe verdict that lands after the
// link recovered must leave the recovery alone.
func TestConnTracker_FailIfOnlyFromState(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := ConnTracker{now: steppingClock(t0)}
	tr.Set(ConnConnected)
	up, _ := tr.Snapshot()
	if tr.FailIf(ConnDisconnected, ConnFailed, errors.New("late verdict")) {
		t.Fatal("FailIf applied from connected, want a no-op")
	}
	if s, _ := tr.Snapshot(); s != up {
		t.Fatalf("mismatched FailIf changed %+v -> %+v", up, s)
	}

	tr.Set(ConnDisconnected)
	if !tr.FailIf(ConnDisconnected, ConnFailed, errors.New("token\nrejected")) {
		t.Fatal("FailIf from the matching state did not apply")
	}
	s, _ := tr.Snapshot()
	if s.State != ConnFailed || s.LastError != "token_rejected" || !s.LastErrorAt.Equal(s.Since) || !s.Since.After(up.Since) {
		t.Fatalf("after FailIf: %+v; want failed with a fresh Since and the sanitised error stamped then", s)
	}
}

// TestConnTracker_ConcurrentUse is for -race: SDK hooks and /health requests
// hit the tracker from different goroutines.
func TestConnTracker_ConcurrentUse(t *testing.T) {
	var tr ConnTracker
	var wg sync.WaitGroup
	kinds := []ConnStateKind{ConnConnecting, ConnConnected, ConnDisconnected, ConnFailed}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch j % 4 {
				case 0:
					tr.Set(kinds[(i+j)%len(kinds)])
				case 1:
					tr.Fail(kinds[(i+j)%len(kinds)], errors.New("boom"))
				case 2:
					tr.NoteError(errors.New("noted"))
				default:
					tr.Snapshot()
				}
			}
		}(i)
	}
	wg.Wait()
	if _, ok := tr.Snapshot(); !ok {
		t.Error("tracker not observable after concurrent Sets")
	}
}

func TestConnStateOf(t *testing.T) {
	up := ConnState{State: ConnConnected}
	cases := []struct {
		name   string
		p      Platform
		want   ConnState
		wantOK bool
	}{
		{"nil", nil, ConnState{}, false},
		{"no reporter", fakePlat{}, ConnState{}, false},
		{"reporter declines", connPlat{s: up, ok: false}, up, false},
		{"reporter answers", connPlat{s: up, ok: true}, up, true},
		{"reporter answers without a state", connPlat{ok: true}, ConnState{}, false},
	}
	for _, c := range cases {
		got, ok := ConnStateOf(c.p)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: ConnStateOf = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestConnStatesOf_KeepsOnlyObservable(t *testing.T) {
	up := ConnState{State: ConnConnected}
	got := ConnStatesOf(map[string]Platform{
		"plain":    fakePlat{},
		"declines": connPlat{s: up},
		"answers":  connPlat{s: up, ok: true},
		"broken":   nil,
	})
	if len(got) != 1 || got["answers"] != up {
		t.Errorf("ConnStatesOf = %+v, want only the answering platform", got)
	}
	if got := ConnStatesOf(map[string]Platform{"plain": fakePlat{}}); got != nil {
		t.Errorf("ConnStatesOf with no reporter = %+v, want nil", got)
	}
}

func TestCapabilitiesOf_ConnStateIsTypeAssertion(t *testing.T) {
	if CapabilitiesOf(fakePlat{}).ConnState {
		t.Error("a platform without ConnStateReporter reports conn_state")
	}
	// Declining at runtime (feishu webhook) is still the capability.
	if !CapabilitiesOf(connPlat{}).ConnState {
		t.Error("a ConnStateReporter is not reported as conn_state")
	}
}
