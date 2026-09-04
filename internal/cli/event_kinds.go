package cli

import "github.com/naozhi/naozhi/internal/cli/clievent"

// IsActivityType reports whether the given EventEntry.Type belongs to the
// "activity" set tracked by EventLog.lastActivitySummary. Shared by EventLog
// (Append / AppendBatch) and session.ManagedSession's history scan so live and
// replay tails agree; eventlog_activity_contract_test.go pins the set.
func IsActivityType(t string) bool {
	switch t {
	case "tool_use", "thinking", "agent", "task_start", "task_progress", "todo":
		return true
	}
	return false
}

// internalEventTypes MUST stay byte-for-byte aligned with INTERNAL_EVENT_TYPES
// in internal/server/static/dashboard.js: the types processEventsForDisplay()
// filters out (no chat bubble). The visible-aware history readers
// (EventLog.LastNVisible, ManagedSession.EventLastNVisibleCtx) count entries
// NOT in this set so the first page always carries renderable messages even
// when an agent team floods the tail with tool_use / task_progress.
// static_ux_contract_test.go pins the two sets together.
var internalEventTypes = map[string]struct{}{
	"tool_use":      {},
	"result":        {},
	"agent":         {},
	"task_start":    {},
	"task_progress": {},
	"task_done":     {},
}

// IsInternalEventType mirrors the dashboard's isInternalEvent(): true means the
// UI filters the entry out of the main transcript (no chat bubble). Distinct
// from IsActivityType, which serves the lastActivity summary and includes
// thinking/todo — do NOT conflate the two sets.
func IsInternalEventType(t string) bool {
	_, ok := internalEventTypes[t]
	return ok
}

// IsVisibleEntry reports whether the dashboard would render this entry as a
// visible chat bubble. The inverse of IsInternalEventType, lifted to the
// EventEntry shape for the visible-aware history readers.
func IsVisibleEntry(e clievent.EventEntry) bool {
	return !IsInternalEventType(e.Type)
}
