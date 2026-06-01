package cli

// IsActivityType reports whether the given EventEntry.Type belongs to the
// "activity" set tracked by EventLog.lastActivitySummary. Both EventLog
// (Append / AppendBatch) and session.ManagedSession (history scan in
// extractLastPromptFromProcess) need the predicate; centralising it here
// removes the previous duplicate definition in session.isActivityType
// whose godoc had to remind future maintainers to keep the two switches in
// sync. R228-CR-3.
//
// The set must stay aligned with the cases that EventLog.Append /
// AppendBatch use to update lastActivitySummary — adding a new type to one
// without updating the other would leave history backfill blind to events
// the live path counts as activity. eventlog_activity_contract_test.go
// pins the set.
func IsActivityType(t string) bool {
	switch t {
	case "tool_use", "thinking", "agent", "task_start", "task_progress", "todo":
		return true
	}
	return false
}

// internalEventTypes MUST stay byte-for-byte aligned with the
// INTERNAL_EVENT_TYPES Set in internal/server/static/dashboard.js. These are
// the event types the dashboard's processEventsForDisplay() filters out — they
// never render a chat bubble. The server's visible-aware history readers
// (EventLog.LastNVisible, ManagedSession.EventLastNVisibleCtx) count entries
// NOT in this set so the initial payload always carries enough renderable
// events; if a parallel agent team floods the trailing window with tool_use /
// task_progress events, the reader keeps walking back until it finds real
// messages instead of handing the dashboard a page that renders to a blank
// "该会话最近仅有 agent 活动" placeholder.
//
// Drift between this set and the JS Set silently breaks that guarantee, so
// static_ux_contract_test.go pins the two together — adding a type to one
// without the other turns CI red.
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
func IsVisibleEntry(e EventEntry) bool {
	return !IsInternalEventType(e.Type)
}
