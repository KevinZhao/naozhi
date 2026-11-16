package agent

import (
	"slices"
	"testing"
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
