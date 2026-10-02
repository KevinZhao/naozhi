package clievent

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

// TestKindTable pins the registry: names are unique and non-empty, and the
// three derived sets are exactly the ones the literal switch / map held before
// the table existed (and the dashboard's two hand-written Sets still hold).
func TestKindTable(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range kindTable {
		if k.Name == "" || seen[k.Name] {
			t.Errorf("kind %q is empty or listed twice", k.Name)
		}
		seen[k.Name] = true
	}
	consts := []string{KindUser, KindText, KindThinking, KindToolUse, KindToolResult, KindAgent, KindTodo,
		KindAskQuestion, KindTaskStart, KindTaskProgress, KindTaskDone, KindResult, KindSystem, KindPersistGap}
	for _, k := range consts {
		if !IsKnownKind(k) {
			t.Errorf("constant %q has no kindTable row", k)
		}
	}
	if got := AllKinds(); len(got) != len(consts) || len(got) != len(kindTable) {
		t.Errorf("AllKinds() = %v, want one row per Kind* constant (%d)", got, len(consts))
	}
	sets := []struct {
		name string
		pred func(string) bool
		list []string
		want []string
	}{
		{"activity", IsActivityType, nil, []string{"tool_use", "thinking", "agent", "task_start", "task_progress", "todo"}},
		{"internal", IsInternalEventType, InternalKinds(), []string{"tool_use", "result", "agent", "task_start", "task_progress", "task_done"}},
		{"markdown-ignore", nil, MarkdownIgnoreKinds(), []string{"tool_use", "result", "agent", "task_start", "task_progress", "task_done", "thinking", "ask_question"}},
	}
	for _, s := range sets {
		want := map[string]bool{}
		for _, w := range s.want {
			want[w] = true
			if !IsKnownKind(w) {
				t.Errorf("%s: %q is not a registered kind", s.name, w)
			}
		}
		for _, k := range append(AllKinds(), "", "init", "unknown_kind") {
			if s.pred != nil && s.pred(k) != want[k] {
				t.Errorf("%s(%q) = %v, want %v", s.name, k, s.pred(k), want[k])
			}
		}
		if s.list != nil {
			got := map[string]bool{}
			for _, k := range s.list {
				got[k] = true
			}
			if len(got) != len(want) || len(s.list) != len(want) {
				t.Errorf("%s list = %v, want %v", s.name, s.list, s.want)
			}
			for w := range want {
				if !got[w] {
					t.Errorf("%s list lacks %q", s.name, w)
				}
			}
		}
	}
	for _, bad := range []string{"", "init", "txt", "Text"} {
		if IsKnownKind(bad) {
			t.Errorf("IsKnownKind(%q) = true", bad)
		}
	}
}
