package server

import (
	"context"
	"errors"
	"path/filepath"
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

// TestPlannerTurnSources: each request counts from its Begin to its own
// Finish, so requests running side by side (passthrough's detached turns)
// are all counted until each finishes; an observer took none.
func TestPlannerTurnSources(t *testing.T) {
	ctx := context.Background()
	s := &Server{hub: &Hub{engine: &sendEngine{}}}
	turns := &s.hub.engine.plannerTurns
	origin := func(src agent.Source) *plannerOrigin {
		return &plannerOrigin{key: agent.Key, src: src, turns: turns, notify: nopNotifier{}}
	}
	user, scout, receipt := origin(agent.SourceUser), origin(agent.SourceScout), origin(agent.SourceReceipt)
	steps := []struct {
		name string
		do   func()
		want []agent.Source
	}{
		{"user's turn begins", func() { user.Begin(ctx, turn.TurnInfo{Role: turn.RoleHead}) }, []agent.Source{agent.SourceUser}},
		{"a scout wake runs beside it", func() { scout.Begin(ctx, turn.TurnInfo{Role: turn.RoleHead}) },
			[]agent.Source{agent.SourceScout, agent.SourceUser}},
		{"an observer takes none", func() { receipt.Begin(ctx, turn.TurnInfo{Role: turn.RoleObserver}) },
			[]agent.Source{agent.SourceScout, agent.SourceUser}},
		// The user's turn ends first; the scout's still runs on the CLI.
		{"user's turn finishes", func() { user.Finish(ctx, turn.Outcome{Stage: turn.StageDone}) }, []agent.Source{agent.SourceScout}},
		{"scout's turn finishes", func() { scout.Finish(ctx, turn.Outcome{Stage: turn.StageDone}) }, nil},
	}
	for _, st := range steps {
		st.do()
		if got := s.PlannerTurnSources(agent.Key); !slices.Equal(got, st.want) {
			t.Errorf("%s: sources = %q, want %q", st.name, got, st.want)
		}
		if got := s.PlannerTurnSources("task:0a1b2c3d4e5f"); len(got) != 0 {
			t.Errorf("%s: other key = %q, want none", st.name, got)
		}
	}
}

// TestPlannerTurnsCarriedOver: a request in flight when the engine shut
// down stays on disk, and the next run counts it until a result answers it.
func TestPlannerTurnsCarriedOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "planner", "turns.json")
	ctx, cancel := context.WithCancel(context.Background())
	var before plannerTurns
	before.load(ctx, path)
	const mark = "4242:5"
	o := &plannerOrigin{key: agent.Key, src: agent.SourceScout, turns: &before, notify: nopNotifier{},
		mark: func(string) string { return mark }}
	o.Begin(ctx, turn.TurnInfo{Role: turn.RoleHead})
	cancel() // shutdown: the shim keeps the turn, the Send gives up
	o.Finish(ctx, turn.Outcome{Err: context.Canceled})

	var after plannerTurns
	after.load(context.Background(), path)
	if got := after.sources(agent.Key); !slices.Equal(got, []agent.Source{agent.SourceScout}) {
		t.Fatalf("carried over = %q, want [scout]", got)
	}
	if got := after.restored[0].After; got != mark {
		t.Errorf("carried-over watermark = %q, want %q: the next run cannot tell this turn's result", got, mark)
	}
	after.resulted("task:0a1b2c3d4e5f")
	if got := after.sources(agent.Key); len(got) != 1 {
		t.Errorf("another key's result answered it: %q", got)
	}
	after.resulted(agent.Key)
	if got := after.sources(agent.Key); len(got) != 0 {
		t.Errorf("after its result = %q, want none", got)
	}
	var again plannerTurns
	again.load(context.Background(), path)
	if got := again.sources(agent.Key); len(got) != 0 {
		t.Errorf("answered request carried over again: %q", got)
	}
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
