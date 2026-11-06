package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
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
		end func(t *testing.T, s *ManagedSession, proc *hookedTestProcess) time.Time
	}{
		{"a turn the CLI started itself", func(t *testing.T, s *ManagedSession, proc *hookedTestProcess) time.Time {
			at := time.Now()
			proc.fn(clievent.SendResult{Text: "后台任务跑完了"})
			return at
		}},
		{"one that ended in an API error", func(t *testing.T, s *ManagedSession, proc *hookedTestProcess) time.Time {
			at := time.Now()
			proc.fn(clievent.SendResult{IsError: true})
			return at
		}},
		{"the reply of a turn a shim held across a restart", func(t *testing.T, s *ManagedSession, proc *hookedTestProcess) time.Time {
			at := time.Now()
			(&Router{}).resultFor(s)("done")
			return at
		}},
		{"a send's turn that failed", func(t *testing.T, s *ManagedSession, proc *hookedTestProcess) time.Time {
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
		{"a passthrough send's turn", func(t *testing.T, s *ManagedSession, proc *hookedTestProcess) time.Time {
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
			proc := &hookedTestProcess{TestProcess: NewTestProcess()}
			s := &ManagedSession{key: "dashboard:direct:host:general"}
			s.storeProcess(proc)
			bookUnownedResults(s, proc)
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
