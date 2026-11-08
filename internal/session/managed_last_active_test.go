package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// Every turn's end refreshes the session's last activity, whoever started
// it: the sidebar row of a session whose CLI answered a background task on
// its own read "5 分钟前". It writes no event, so custody's "you are nearby"
// and loop guard, which read the user's entries, do not take it for the
// user's message.
func TestTurnEndRefreshesLastActive(t *testing.T) {
	failed := errors.New("turn failed")
	cases := []struct {
		name string
		// end ends a turn on s and returns when it ended.
		end func(t *testing.T, s *ManagedSession, proc *TestProcess) time.Time
	}{
		{"a turn the CLI started itself", func(t *testing.T, s *ManagedSession, proc *TestProcess) time.Time {
			at := time.Now()
			(&Router{}).resultFor(s)(clievent.SendResult{Text: "后台任务跑完了"})
			return at
		}},
		{"one that ended in an API error", func(t *testing.T, s *ManagedSession, proc *TestProcess) time.Time {
			at := time.Now()
			(&Router{}).resultFor(s)(clievent.SendResult{IsError: true})
			return at
		}},
		{"an aborted one", func(t *testing.T, s *ManagedSession, proc *TestProcess) time.Time {
			at := time.Now()
			(&Router{}).resultFor(s)(clievent.SendResult{Aborted: true})
			return at
		}},
		{"a send's turn that failed", func(t *testing.T, s *ManagedSession, proc *TestProcess) time.Time {
			var at time.Time
			proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
				at = time.Now()
				return nil, failed
			}
			if _, err := s.Send(context.Background(), "hi", nil, nil); !errors.Is(err, failed) {
				t.Fatalf("Send = %v, want %v", err, failed)
			}
			return at
		}},
		{"a passthrough send's turn", func(t *testing.T, s *ManagedSession, proc *TestProcess) time.Time {
			var at time.Time
			proc.SendPassthroughFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback, string) (*clievent.SendResult, error) {
				at = time.Now()
				return &clievent.SendResult{Text: "ok"}, nil
			}
			if _, err := s.SendPassthrough(context.Background(), "hi", nil, nil, ""); err != nil {
				t.Fatal(err)
			}
			return at
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proc := NewTestProcess()
			s := &ManagedSession{key: "dashboard:direct:host:general"}
			s.storeProcess(proc)
			s.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())

			ended := c.end(t, s, proc)
			if got := s.LastActive(); got.Before(ended) {
				t.Errorf("last active = %v, want at or after the turn's end %v", got, ended)
			}
			if n := len(proc.EventLog.Entries()); n != 0 {
				t.Errorf("event log holds %d entries, want none written for the turn's end", n)
			}
		})
	}
}

// The turn a shim held across a restart ends on the read loop with no Send to
// return: its end refreshes the last activity whether it answered or failed,
// or custody, which dates the failure by it, takes a long turn's failure for
// an old one. Only the answer reaches the result sink.
func TestReattachedTurnEndRefreshesLastActive(t *testing.T) {
	for _, tc := range []struct {
		name, result, sunk string
	}{
		{"answered", `{"type":"result","subtype":"success","result":"done","session_id":"s1"}`, "done"},
		{"failed", `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "dashboard:direct:host:general"
			r, proc, emit := reconnectToFakeShim(t, key, 1, []string{adoptAssistantLine})
			sunk := make(chan string, 4)
			r.SetResultSink(func(_, text string) { sunk <- text })
			s := r.ss.Load(key)
			s.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())

			ended := time.Now()
			emit(tc.result)
			// The latch resolves after the result hook has run.
			testhelper.Eventually(t, func() bool { return !proc.AdoptedTurnPending() },
				5*time.Second, "the live result never arrived")
			if got := s.LastActive(); got.Before(ended) {
				t.Errorf("last active = %v, want at or after the turn's end %v", got, ended)
			}
			select {
			case text := <-sunk:
				if text != tc.sunk {
					t.Errorf("result sink got %q, want %q", text, tc.sunk)
				}
			default:
				if tc.sunk != "" {
					t.Errorf("result sink got nothing, want %q", tc.sunk)
				}
			}
		})
	}
}

// A turn running for hours keeps last_active moving with its events, a
// minute's step at a time: the list, index.json and the Planner's snapshot
// read it, and an event inside the step leaves it where it was.
func TestLastActiveFollowsEvents(t *testing.T) {
	proc := NewTestProcess()
	proc.StateVal = cli.StateRunning
	s := &ManagedSession{key: "dashboard:direct:la:general"}
	s.storeProcess(proc)

	started := time.Now().Add(-3 * time.Hour)
	s.lastActive.Store(started.UnixNano())
	if got := s.LastActive(); !got.Equal(started) {
		t.Fatalf("before any event: %v, want the turn's start %v", got, started)
	}

	proc.EventLog.Append(clievent.EventEntry{Type: "tool_use", Summary: "Bash"})
	ev := proc.LastEventAt()
	if got := s.LastActive(); !got.Equal(ev) {
		t.Fatalf("after an event 3h into the turn: %v, want the event's %v", got, ev)
	}
	if got := s.Snapshot().LastActive; got != ev.UnixMilli() {
		t.Errorf("snapshot last_active = %d, want %d", got, ev.UnixMilli())
	}

	// Within the step: the stored value stays.
	within := time.Now().Add(-30 * time.Second)
	s.lastActive.Store(within.UnixNano())
	proc.EventLog.Append(clievent.EventEntry{Type: "text", Summary: "still going"})
	if got := s.LastActive(); !got.Equal(within) {
		t.Errorf("event %v after the last refresh moved it: %v", time.Since(within).Round(time.Second), got)
	}
	if got := toStoreEntryLastActive(t, s); got != within.UnixNano() {
		t.Errorf("stored last_active = %d, want %d", got, within.UnixNano())
	}

	// Never backwards: a turn's end after the last event stays.
	end := time.Now().Add(time.Minute)
	s.lastActive.Store(end.UnixNano())
	if got := s.LastActive(); !got.Equal(end) {
		t.Errorf("last_active went back to an older event: %v, want %v", got, end)
	}
}

func toStoreEntryLastActive(t *testing.T, s *ManagedSession) int64 {
	t.Helper()
	s.setSessionID("sid-la")
	e, ok := sessionToStoreEntry(s)
	if !ok {
		t.Fatal("no store entry")
	}
	return e.LastActive
}
