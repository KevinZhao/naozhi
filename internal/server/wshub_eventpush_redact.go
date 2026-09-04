// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     none (pure function over []clievent.EventEntry; no Hub field access)
//	READS:      none
package server

import (
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// redactEntrySecrets returns a view of entries with credential token shapes
// scrubbed from the free-text Summary and Detail fields.
//
// Copy-on-write: the EventEntry values live in EventLog's shared ring buffer,
// read concurrently by other subscribers, so the slice and its entries MUST
// NOT be mutated in place. The input is aliased until the first entry that
// actually changes; only then is the slice cloned. Clean output (the common
// case) allocates nothing; textutil.RedactSecrets is itself zero-alloc when
// no prefix matches.
func redactEntrySecrets(entries []clievent.EventEntry) []clievent.EventEntry {
	out := entries // alias until a mutation forces a clone
	for i := range entries {
		summary := textutil.RedactSecrets(entries[i].Summary)
		detail := textutil.RedactSecrets(entries[i].Detail)
		if summary == entries[i].Summary && detail == entries[i].Detail {
			continue
		}
		if &out[0] == &entries[0] {
			// First change: clone the slice so we never write through the
			// alias into EventLog's shared buffer.
			out = make([]clievent.EventEntry, len(entries))
			copy(out, entries)
		}
		out[i].Summary = summary
		out[i].Detail = detail
	}
	return out
}
