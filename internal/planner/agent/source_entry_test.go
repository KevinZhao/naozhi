package agent

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// The Planner page folds a turn into 动态 by its entry's source alone
// (§15.14 ②, D113): your turns (your message, a session you handed over,
// your answer) never fold, nor does the shift change, its separator; every
// turn the gateway, a session or outside content started does. A source is
// a turn of its own, never merged with the user's message (Alone), when its
// limits differ from a user turn's: outside content (scout, briefing, a
// lookup's answer, a watched session that read outside content, the receipts
// of what those were refused), a watched session's turn end (D138) and the
// patrol's findings and receipts (§15.23 ③: the user's own repository, so
// not outside).
func TestSourceEntryAndDynamic(t *testing.T) {
	dynamic := DynamicSources()
	for _, tc := range []struct {
		src            Source
		entry          string
		folds          bool
		alone, outside bool
	}{
		{SourceUser, "", false, false, false},
		{SourceHandover, "", false, false, false},
		{SourceAnswer, "answer", false, false, false},
		{SourceShift, "shift", false, false, false},
		{SourceReceipt, "receipt", true, false, false},
		{SourceReminder, "reminder", true, false, false},
		{SourceScout, "scout", true, true, true},
		{SourceBriefing, "briefing", true, true, true},
		{SourceRefusal, "refusal", true, true, true},
		{SourceUnrun, "unrun", true, false, false},
		{SourceLookup, "lookup", true, true, true},
		{SourceMorning, "morning", true, false, false},
		{SourceWatch, "watch", true, true, false},
		{SourceWatchOutside, "watch", true, true, true},
		{SourceWatchRefusal, "refusal", true, true, true},
		{SourcePatrol, "patrol", true, true, false},
		{SourcePatrolRefusal, "refusal", true, true, false},
		{SourcePatrolReceipt, "receipt", true, true, false},
	} {
		if got := tc.src.Entry(); got != tc.entry {
			t.Errorf("%s.Entry() = %q, want %q", tc.src, got, tc.entry)
		}
		if got := slices.Contains(dynamic, tc.src.Entry()); got != tc.folds {
			t.Errorf("%s folds into 动态 = %v, want %v", tc.src, got, tc.folds)
		}
		if tc.src.Alone() != tc.alone || tc.src.Outside() != tc.outside {
			t.Errorf("%s: alone %v outside %v, want %v %v", tc.src, tc.src.Alone(), tc.src.Outside(), tc.alone, tc.outside)
		}
	}
	if !slices.IsSorted(dynamic) {
		t.Errorf("DynamicSources() = %q, not sorted", dynamic)
	}
}

// TestTurnSourceJudgement: the turn in flight is judged by every source of
// its requests, which is what the action runner limits it by. Outside
// content names it; a receipt of blocks not run (outside or not) makes it
// that receipt's own turn, so what was refused gets no receipt again; a
// watch turn (D138), a patrol turn and a receipt about a session the patrol
// dispatched (§15.23 ③) are each told apart; the morning tidy is what the
// brief waits on (§15.18). Before Start nothing is in flight.
func TestTurnSourceJudgement(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	if a.OutsideTurn() != "" || a.UnrunTurn() || a.WatchTurn() || a.PatrolTurn() || a.PatrolTaskTurn() || a.Tidying() {
		t.Fatal("a turn judged before Start")
	}
	turns := &fakeTurns{}
	a.turns = turns
	for _, tc := range []struct {
		sources              []Source
		outside              string
		unrun, watch, patrol bool
		patrolTask, tidying  bool
	}{
		{sources: nil},
		{sources: []Source{SourceReceipt, SourceUser}},
		{sources: []Source{SourceReminder}},
		{sources: []Source{SourceUnrun, SourceUser}, unrun: true},
		{sources: []Source{SourceRefusal}, outside: "被拒动作的回执", unrun: true},
		{sources: []Source{SourceBriefing, SourceScout}, outside: "会前简报、巡检"},
		{sources: []Source{SourceLookup}, outside: "网页查询结果"},
		{sources: []Source{SourceMorning}, tidying: true},
		{sources: []Source{SourceWatch}, watch: true},
		{sources: []Source{SourceWatchOutside}, outside: "读过外部内容的会话", watch: true},
		{sources: []Source{SourceWatchRefusal}, outside: "被拒动作的回执", unrun: true, watch: true},
		{sources: []Source{SourcePatrol}, patrol: true},
		{sources: []Source{SourceUser, SourcePatrol}, patrol: true},
		{sources: []Source{SourcePatrolRefusal}, unrun: true, patrol: true},
		{sources: []Source{SourcePatrolReceipt}, patrolTask: true},
	} {
		turns.sources = tc.sources
		if got := a.OutsideTurn(); got != tc.outside || a.UnrunTurn() != tc.unrun || a.WatchTurn() != tc.watch ||
			a.PatrolTurn() != tc.patrol || a.PatrolTaskTurn() != tc.patrolTask || a.Tidying() != tc.tidying {
			t.Errorf("%v: outside %q unrun %v watch %v patrol %v patrol task %v tidying %v", tc.sources, got,
				a.UnrunTurn(), a.WatchTurn(), a.PatrolTurn(), a.PatrolTaskTurn(), a.Tidying())
		}
	}
}

// A due promise's wake (wireup.PromiseReminder) is a turn from
// SourceReminder with no user words: not outside content, so its dispatch
// and status run (§15.6 limits only scout, briefing, refusal and lookup
// turns), and not the user's, so relay, remember and the like are refused
// (D98). Wake errors when the Planner cannot take it, for the caller to owe.
func TestReminderTurnFooting(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	if err := a.Wake(SourceReminder, nil, "<naozhi-reminder>x</naozhi-reminder>"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("wake before Start: %v, want ErrNotReady", err)
	}
	turns := &fakeTurns{sources: []Source{SourceReminder}}
	a.turns = turns
	if err := a.Wake(SourceReminder, nil, "<naozhi-reminder>x</naozhi-reminder>"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(turns.srcs, []Source{SourceReminder}) || turns.said[0] != nil {
		t.Errorf("submitted from %q with words %v", turns.srcs, turns.said[0])
	}
	if err := a.SetStopped(true); err != nil {
		t.Fatal(err)
	}
	if err := a.Wake(SourceReminder, nil, "<naozhi-reminder>x</naozhi-reminder>"); !errors.Is(err, ErrStopped) {
		t.Errorf("wake while stopped: %v, want ErrStopped", err)
	}
}

// The receipt of a turn's blocks not run starts a turn from its own source,
// with no user words: a watch turn's (TellWatchRefused) is a watch turn, an
// outside one and a receipt's own; a patrol turn's (TellPatrolRefused) a
// patrol turn and a receipt's own. So what was refused is not run there
// either and gets no receipt, and the two do not go round.
func TestRefusedReceipts(t *testing.T) {
	for _, tc := range []struct {
		tell func(*Agent, string) error
		src  Source
	}{
		{(*Agent).TellRefused, SourceRefusal},
		{(*Agent).TellUnrun, SourceUnrun},
		{(*Agent).TellWatchRefused, SourceWatchRefusal},
		{(*Agent).TellPatrolRefused, SourcePatrolRefusal},
	} {
		now := time.Now()
		a, _ := newTestAgent(t, &fakeSessions{}, &now)
		turns := &fakeTurns{}
		a.turns = turns
		if err := tc.tell(a, "第 1 块（dispatch）没有执行"); err != nil {
			t.Fatal(err)
		}
		if len(turns.sent) != 1 || turns.srcs[0] != tc.src || turns.said[0] != nil ||
			turns.sent[0] != "<naozhi-receipt>\n第 1 块（dispatch）没有执行\n</naozhi-receipt>" {
			t.Errorf("sent %q from %q with words %v, want the receipt from %s", turns.sent, turns.srcs, turns.said, tc.src)
		}
	}
}
