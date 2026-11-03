package server

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
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
