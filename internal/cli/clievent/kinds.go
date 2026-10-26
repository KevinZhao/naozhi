// kinds.go — the EventEntry.Type vocabulary and its classification (#2545 G1).
//
// Moved out of internal/cli: these are pure predicates over EventEntry.Type with
// no dependency on the process manager, and their callers are the event-log ring
// (internal/eventlog/ring), internal/session and internal/server. Leaving them in
// cli meant a package that only wanted to know "does the dashboard render this
// entry" had to import the subprocess spawner.
package clievent

// The kinds an EventEntry.Type may hold. Outside this package spell a kind
// with one of these (or copy another entry's Type) rather than a string
// literal, so a typo cannot become a kind.
const (
	KindUser         = "user"
	KindText         = "text"
	KindThinking     = "thinking"
	KindToolUse      = "tool_use"
	KindToolResult   = "tool_result"
	KindAgent        = "agent"
	KindTodo         = "todo"
	KindAskQuestion  = "ask_question"
	KindTaskStart    = "task_start"
	KindTaskProgress = "task_progress"
	KindTaskDone     = "task_done"
	KindResult       = "result"
	KindSystem       = "system"
	KindPersistGap   = "persist_gap"
)

// KindInfo classifies one kind. Internal: the dashboard filters it out of the
// transcript (no chat bubble) unless a view asks for internal events, and the
// visible-aware history readers (EventLog.LastNVisible,
// ManagedSession.EventLastNVisibleCtx) do not count it, so a first page
// flooded by an agent team still carries renderable messages. NoBubble: not
// Internal (internal-event filters keep it), but the dashboard renders it as
// nothing in every view, so those readers do not count it either.
// Activity: it updates EventLog.lastActivitySummary, the "what is the agent
// doing" tail live appends and the history replay scan must agree on.
// MarkdownIgnore: the dashboard's markdown export leaves it out.
type KindInfo struct {
	Name           string
	Internal       bool
	NoBubble       bool
	Activity       bool
	MarkdownIgnore bool
}

// kindTable is the single registry of kinds; kinds_test.go pins its sets. A
// new kind that is not NoBubble also needs an EVENT_WHOLE or EVENT_CONTENT
// entry in the dashboard.
var kindTable = []KindInfo{
	{Name: KindUser},
	{Name: KindText},
	{Name: KindThinking, NoBubble: true, Activity: true, MarkdownIgnore: true},
	{Name: KindToolUse, Internal: true, Activity: true, MarkdownIgnore: true},
	{Name: KindToolResult},
	{Name: KindAgent, Internal: true, Activity: true, MarkdownIgnore: true},
	{Name: KindTodo, Activity: true},
	{Name: KindAskQuestion, MarkdownIgnore: true},
	{Name: KindTaskStart, Internal: true, Activity: true, MarkdownIgnore: true},
	{Name: KindTaskProgress, Internal: true, Activity: true, MarkdownIgnore: true},
	{Name: KindTaskDone, Internal: true, MarkdownIgnore: true},
	{Name: KindResult, Internal: true, MarkdownIgnore: true},
	{Name: KindSystem},
	{Name: KindPersistGap},
}

var kindByName = func() map[string]KindInfo {
	m := make(map[string]KindInfo, len(kindTable))
	for _, k := range kindTable {
		m[k.Name] = k
	}
	return m
}()

func kindNames(keep func(KindInfo) bool) []string {
	var out []string
	for _, k := range kindTable {
		if keep(k) {
			out = append(out, k.Name)
		}
	}
	return out
}

// AllKinds returns every registered kind, in table order.
func AllKinds() []string { return kindNames(func(KindInfo) bool { return true }) }

// InternalKinds returns the kinds the dashboard keeps out of the transcript.
func InternalKinds() []string { return kindNames(func(k KindInfo) bool { return k.Internal }) }

// NoBubbleKinds returns the kinds the dashboard renders as nothing.
func NoBubbleKinds() []string { return kindNames(func(k KindInfo) bool { return k.NoBubble }) }

// MarkdownIgnoreKinds returns the kinds the markdown export leaves out.
func MarkdownIgnoreKinds() []string {
	return kindNames(func(k KindInfo) bool { return k.MarkdownIgnore })
}

// IsActivityType reports whether an entry of type t updates the lastActivity
// summary (KindInfo.Activity). Distinct from KindInfo.Internal: thinking and
// todo are activity yet not internal, result and task_done are internal yet not
// activity — do NOT conflate the two sets.
func IsActivityType(t string) bool { return kindByName[t].Activity }

// IsVisibleEntry reports whether the dashboard would render this entry as a
// visible chat bubble in the main transcript: neither Internal nor NoBubble.
// The visible-aware history readers count with it. An unregistered type
// counts as visible (the dashboard renders it as the unknown-type chip).
func IsVisibleEntry(e EventEntry) bool {
	k := kindByName[e.Type]
	return !k.Internal && !k.NoBubble
}
