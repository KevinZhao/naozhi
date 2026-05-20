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
