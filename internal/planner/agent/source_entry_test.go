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
// turn the gateway, a session or outside content started does.
func TestSourceEntryAndDynamic(t *testing.T) {
	dynamic := DynamicSources()
	for _, tc := range []struct {
		src   Source
		entry string
		folds bool
	}{
		{SourceUser, "", false},
		{SourceHandover, "", false},
		{SourceAnswer, "answer", false},
		{SourceShift, "shift", false},
		{SourceReceipt, "receipt", true},
		{SourceReminder, "reminder", true},
		{SourceScout, "scout", true},
		{SourceBriefing, "briefing", true},
		{SourceRefusal, "refusal", true},
		{SourceUnrun, "unrun", true},
		{SourceLookup, "lookup", true},
		{SourceMorning, "morning", true},
		{SourceWatch, "watch", true},
		{SourceWatchOutside, "watch", true},
		{SourceWatchRefusal, "refusal", true},
		{SourcePatrol, "patrol", true},
		{SourcePatrolRefusal, "refusal", true},
		{SourcePatrolReceipt, "receipt", true},
	} {
		if got := tc.src.Entry(); got != tc.entry {
			t.Errorf("%s.Entry() = %q, want %q", tc.src, got, tc.entry)
		}
		if got := slices.Contains(dynamic, tc.src.Entry()); got != tc.folds {
			t.Errorf("%s folds into 动态 = %v, want %v", tc.src, got, tc.folds)
		}
	}
	if !slices.IsSorted(dynamic) {
		t.Errorf("DynamicSources() = %q, not sorted", dynamic)
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
	if SourceReminder.Outside() || a.OutsideTurn() != "" || a.UnrunTurn() || len(a.TurnWords()) != 0 {
		t.Errorf("reminder turn: outside %q, unrun %v, words %v", a.OutsideTurn(), a.UnrunTurn(), a.TurnWords())
	}
	if err := a.SetStopped(true); err != nil {
		t.Fatal(err)
	}
	if err := a.Wake(SourceReminder, nil, "<naozhi-reminder>x</naozhi-reminder>"); !errors.Is(err, ErrStopped) {
		t.Errorf("wake while stopped: %v, want ErrStopped", err)
	}
}

// The morning tidy's turn (brief.Config.Tidy) stands as a reminder's does:
// the gateway's list of sessions, not outside content, so its front blocks
// run; and not the user's. Tidying is what the brief waits on.
func TestMorningTurnFooting(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	if SourceMorning.Outside() {
		t.Fatal("the morning tidy is judged as outside content: its front blocks would be refused")
	}
	if a.Tidying() {
		t.Fatal("tidying before Start")
	}
	turns := &fakeTurns{sources: []Source{SourceMorning}}
	a.turns = turns
	if err := a.Wake(SourceMorning, nil, "<naozhi-morning>x</naozhi-morning>"); err != nil {
		t.Fatal(err)
	}
	if len(turns.srcs) != 1 || turns.srcs[0] != SourceMorning || turns.said[0] != nil {
		t.Fatalf("submitted from %v with words %v", turns.srcs, turns.said)
	}
	if !a.Tidying() || a.OutsideTurn() != "" || len(a.TurnWords()) != 0 {
		t.Fatalf("tidying %v, outside %q, words %v", a.Tidying(), a.OutsideTurn(), a.TurnWords())
	}
	turns.sources = nil
	if a.Tidying() {
		t.Fatal("tidying once the turn is over")
	}
}

// A watched session's turn end starts a turn of its own, never merged with
// the user's message (Alone), and one of a session that read outside content
// is an outside turn as well: its limits ride with the source into
// turns.json. WatchTurn is what the action runner judges such a turn by. The
// receipt of a watch turn's blocks not run (TellWatchRefused) starts a turn
// that is all three: a watch turn, an outside one and a receipt's own, so
// what the watch turn was refused is not run there either and gets no
// receipt.
func TestWatchTurnFooting(t *testing.T) {
	for _, tc := range []struct {
		src            Source
		alone, outside bool
	}{
		{SourceUser, false, false},
		{SourceReceipt, false, false},
		{SourceUnrun, false, false},
		{SourceScout, true, true},
		{SourceRefusal, true, true},
		{SourceWatch, true, false},
		{SourceWatchOutside, true, true},
		{SourceWatchRefusal, true, true},
	} {
		if tc.src.Alone() != tc.alone || tc.src.Outside() != tc.outside {
			t.Errorf("%s: alone %v outside %v, want %v %v", tc.src, tc.src.Alone(), tc.src.Outside(), tc.alone, tc.outside)
		}
	}
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	if a.WatchTurn() {
		t.Fatal("a watch turn before Start")
	}
	turns := &fakeTurns{}
	a.turns = turns
	for _, tc := range []struct {
		sources []Source
		watch   bool
		outside string
		unrun   bool
	}{
		{nil, false, "", false},
		{[]Source{SourceReceipt}, false, "", false},
		{[]Source{SourceUnrun}, false, "", true},
		{[]Source{SourceRefusal}, false, "被拒动作的回执", true},
		{[]Source{SourceWatch}, true, "", false},
		{[]Source{SourceWatchOutside}, true, "读过外部内容的会话", false},
		{[]Source{SourceWatchRefusal}, true, "被拒动作的回执", true},
	} {
		turns.sources = tc.sources
		if a.WatchTurn() != tc.watch || a.OutsideTurn() != tc.outside || a.UnrunTurn() != tc.unrun || len(a.TurnWords()) != 0 {
			t.Errorf("%v: watch %v outside %q unrun %v, want %v %q %v", tc.sources, a.WatchTurn(), a.OutsideTurn(), a.UnrunTurn(), tc.watch, tc.outside, tc.unrun)
		}
	}
	turns.sources = nil
	if err := a.TellWatchRefused("第 1 块（dispatch）没有执行"); err != nil {
		t.Fatal(err)
	}
	if len(turns.sent) != 1 || turns.srcs[0] != SourceWatchRefusal || turns.said[0] != nil ||
		turns.sent[0] != "<naozhi-receipt>\n第 1 块（dispatch）没有执行\n</naozhi-receipt>" {
		t.Errorf("sent %q from %q with words %v, want the receipt from watch_refusal", turns.sent, turns.srcs, turns.said)
	}
}

// A deep check's new findings start a turn of their own, never merged with
// the user's message (Alone), and not an outside one: they are the user's
// own repository's content, so the turn may dispatch (§15.23 ③). PatrolTurn
// is what the action runner limits such a turn by. The receipt of its
// blocks not run (TellPatrolRefused) starts a turn that is a patrol turn
// and a receipt's own: the same limits and caps hold, and it gets no
// receipt, so the two do not go round.
func TestPatrolTurnFooting(t *testing.T) {
	for _, src := range []Source{SourcePatrol, SourcePatrolRefusal} {
		if !src.Alone() || src.Outside() {
			t.Errorf("%s: alone %v outside %v, want alone and not outside", src, src.Alone(), src.Outside())
		}
	}
	now := time.Now()
	a, _ := newTestAgent(t, &fakeSessions{}, &now)
	if a.PatrolTurn() {
		t.Fatal("a patrol turn before Start")
	}
	turns := &fakeTurns{}
	a.turns = turns
	for _, tc := range []struct {
		sources       []Source
		patrol, unrun bool
	}{
		{nil, false, false},
		{[]Source{SourceReceipt}, false, false},
		{[]Source{SourceUnrun}, false, true},
		{[]Source{SourceScout}, false, false},
		{[]Source{SourceWatch}, false, false},
		{[]Source{SourcePatrol}, true, false},
		{[]Source{SourcePatrolRefusal}, true, true},
		{[]Source{SourceUser, SourcePatrol}, true, false},
	} {
		turns.sources = tc.sources
		if a.PatrolTurn() != tc.patrol || a.UnrunTurn() != tc.unrun {
			t.Errorf("%v: patrol %v unrun %v, want %v %v", tc.sources, a.PatrolTurn(), a.UnrunTurn(), tc.patrol, tc.unrun)
		}
		if slices.Contains(tc.sources, SourcePatrol) && (a.OutsideTurn() != "" || a.WatchTurn()) {
			t.Errorf("%v: outside %q watch %v, want neither", tc.sources, a.OutsideTurn(), a.WatchTurn())
		}
	}
	turns.sources = nil
	if err := a.Wake(SourcePatrol, nil, `<naozhi-patrol workspace="ws_ab12cd34" name="gateway" run="r1" findings="1">x</naozhi-patrol>`); err != nil {
		t.Fatal(err)
	}
	if len(turns.srcs) != 1 || turns.srcs[0] != SourcePatrol || turns.said[0] != nil {
		t.Fatalf("woken from %q with words %v, want patrol and none", turns.srcs, turns.said)
	}
	if err := a.TellPatrolRefused("第 1 块（status）没有执行"); err != nil {
		t.Fatal(err)
	}
	if len(turns.sent) != 2 || turns.srcs[1] != SourcePatrolRefusal || turns.said[1] != nil ||
		turns.sent[1] != "<naozhi-receipt>\n第 1 块（status）没有执行\n</naozhi-receipt>" {
		t.Errorf("sent %q from %q with words %v, want the receipt from patrol_refusal", turns.sent, turns.srcs, turns.said)
	}
}
