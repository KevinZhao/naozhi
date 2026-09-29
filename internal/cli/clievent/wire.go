package clievent

import "github.com/naozhi/naozhi/internal/textutil"

// ForWire returns the view of entries that leaves the process — a WS frame,
// a REST response, a reverse-node message. Two things differ from the stored
// entry: the agent-linkage fields only the local SubagentLinker reads
// (TaskType, InternalAgentID, JSONLPath, FirstPromptID) are cleared, and
// credential token shapes in Summary and Detail are redacted.
//
// Copy-on-write: entries usually alias EventLog's shared ring buffer, read
// concurrently by other subscribers, so they are never modified. The input
// is returned as is until the first entry that changes, so clean input (the
// common case) is not copied.
func ForWire(entries []EventEntry) []EventEntry {
	out := entries
	cloned := false
	for i := range entries {
		e, changed := wireView(entries[i])
		if !changed {
			continue
		}
		if !cloned {
			out = make([]EventEntry, len(entries))
			copy(out, entries)
			cloned = true
		}
		out[i] = e
	}
	return out
}

// ForWireOne is ForWire for a single entry; nil stays nil. The result never
// aliases e.
func ForWireOne(e *EventEntry) *EventEntry {
	if e == nil {
		return nil
	}
	v, _ := wireView(*e)
	return &v
}

func wireView(e EventEntry) (EventEntry, bool) {
	changed := false
	if e.TaskType != "" || e.InternalAgentID != "" || e.JSONLPath != "" || e.FirstPromptID != "" {
		e.TaskType, e.InternalAgentID, e.JSONLPath, e.FirstPromptID = "", "", "", ""
		changed = true
	}
	if s := textutil.RedactSecrets(e.Summary); s != e.Summary {
		e.Summary = s
		changed = true
	}
	if d := textutil.RedactSecrets(e.Detail); d != e.Detail {
		e.Detail = d
		changed = true
	}
	return e, changed
}
