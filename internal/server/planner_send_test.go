package server

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestPrepareSend_RefusesPlannerNamespace: a dashboard or relayed send must
// not create planner:main as an ordinary session without its launch spec.
func TestPrepareSend_RefusesPlannerNamespace(t *testing.T) {
	e := &sendEngine{}
	for _, key := range []string{"planner:main", "planner:other"} {
		if _, _, err := e.prepareSend(sendParams{Key: key, Text: "hi"}); !errors.Is(err, errPlannerNamespace) {
			t.Errorf("prepareSend(%q) err = %v, want errPlannerNamespace", key, err)
		}
	}
	// task: is reserved too, but a dispatched session takes your messages.
	for _, tc := range []struct {
		existing bool
		want     error
	}{{false, errTaskNamespace}, {true, nil}} {
		e := &sendEngine{router: taskSendRouter{existing: tc.existing}}
		if _, _, err := e.prepareSend(sendParams{Key: "task:0a1b2c3d4e5f", Text: "hi"}); !errors.Is(err, tc.want) {
			t.Errorf("prepareSend(task:, existing=%v) err = %v, want %v", tc.existing, err, tc.want)
		}
	}
}

// TestPrepareSend_RefusesScoutNamespace: only a scout run talks to its
// session.
func TestPrepareSend_RefusesScoutNamespace(t *testing.T) {
	e := &sendEngine{router: taskSendRouter{existing: true}}
	if _, _, err := e.prepareSend(sendParams{Key: "scout:20261105T100000-ab12", Text: "hi"}); !errors.Is(err, errScoutNamespace) {
		t.Errorf("prepareSend(scout:) err = %v, want errScoutNamespace", err)
	}
}

// TestPlannerTurnSources: a turn's sources are those of the requests it
// took, from the first Begin to the first Finish; an observer took none.
func TestPlannerTurnSources(t *testing.T) {
	ctx := context.Background()
	s := &Server{hub: &Hub{engine: &sendEngine{}}}
	turns := &s.hub.engine.plannerTurns
	origin := func(src agent.Source) *plannerOrigin {
		return &plannerOrigin{key: agent.Key, src: src, turns: turns, notify: nopNotifier{}}
	}
	user, scout, receipt := origin(agent.SourceUser), origin(agent.SourceScout), origin(agent.SourceReceipt)
	user.Begin(ctx, turn.TurnInfo{Role: turn.RoleHead})
	scout.Begin(ctx, turn.TurnInfo{Role: turn.RoleHead})
	receipt.Begin(ctx, turn.TurnInfo{Role: turn.RoleObserver})
	if got := s.PlannerTurnSources(agent.Key); !slices.Equal(got, []agent.Source{agent.SourceScout, agent.SourceUser}) {
		t.Errorf("in flight = %q, want [scout user]", got)
	}
	if got := s.PlannerTurnSources("task:0a1b2c3d4e5f"); len(got) != 0 {
		t.Errorf("other key = %q, want none", got)
	}
	user.Finish(ctx, turn.Outcome{Stage: turn.StageDone})
	if got := s.PlannerTurnSources(agent.Key); len(got) != 0 {
		t.Errorf("after the turn = %q, want none", got)
	}
	scout.Finish(ctx, turn.Outcome{Stage: turn.StageDone})
}

// taskSendRouter reports whether a session exists; prepareSend reaches no
// other method for a task: key without a workspace.
type taskSendRouter struct {
	sendEngineRouter
	existing bool
}

func (r taskSendRouter) SessionFor(string) *session.ManagedSession {
	if !r.existing {
		return nil
	}
	return &session.ManagedSession{}
}

// plannerNoteRouter hands out one session for every key.
type plannerNoteRouter struct {
	fakeServerRouter
	sess *session.ManagedSession
}

func (r *plannerNoteRouter) SessionFor(string) *session.ManagedSession { return r.sess }

// TestPlannerNote_LandsInHistory: a promise reminder must stay in the
// Planner's conversation for a page opened after it went out.
func TestPlannerNote_LandsInHistory(t *testing.T) {
	sess := &session.ManagedSession{}
	s := &Server{router: &plannerNoteRouter{sess: sess}}
	s.PlannerNote("planner:main", "到点提醒：交周报")
	got := sess.EventEntries()
	if len(got) != 1 || got[0].Type != clievent.KindSystem || got[0].Summary != "到点提醒：交周报" {
		t.Fatalf("history = %+v, want one system event with the note", got)
	}
}
