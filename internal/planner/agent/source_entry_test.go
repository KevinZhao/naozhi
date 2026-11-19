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
