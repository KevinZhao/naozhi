package cli

import "testing"

// TestIsActivityType_Set locks the activity-type set against accidental
// drift. Both EventLog.Append/AppendBatch and session.scanLastSummaries
// route through IsActivityType — divergence here would let the live
// "what's the agent doing" tail (lastActivitySummary) and the history
// replay tail disagree about what counts as activity.
func TestIsActivityType_Set(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		want bool
	}{
		{"tool_use", "tool_use", true},
		{"thinking", "thinking", true},
		{"agent", "agent", true},
		{"task_start", "task_start", true},
		{"task_progress", "task_progress", true},
		{"todo", "todo", true},

		// Non-activity events: must NOT bump lastActivitySummary.
		{"user", "user", false},
		{"text", "text", false},
		{"result", "result", false},
		{"system", "system", false},
		{"task_done", "task_done", false},
		{"init", "init", false},
		{"ask_question", "ask_question", false},
		{"empty", "", false},
		{"unknown", "unknown_kind", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsActivityType(tc.typ); got != tc.want {
				t.Errorf("IsActivityType(%q) = %v, want %v", tc.typ, got, tc.want)
			}
		})
	}
}
