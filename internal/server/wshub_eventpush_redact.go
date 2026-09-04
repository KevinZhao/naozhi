// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     none (pure function over []clievent.EventEntry; no Hub field access)
//	READS:      none
//
// 与 wshub_types.go 同类——rule 3a/3b 的字段块对账对纯 helper 不适用，
// 但 marker 仍需声明以满足 Phase 0b 的 marker-existence gate。
package server

import (
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// redactEntrySecrets returns a view of entries with credential token shapes
// scrubbed from the free-text Summary and Detail fields (R20260604-SEC-10).
//
// Copy-on-write: the EventEntry values live in EventLog's shared ring buffer,
// which other subscribers read concurrently, so the slice and its entries MUST
// NOT be mutated in place. The function aliases the input slice until it finds
// the first entry whose Summary or Detail actually changes; only then does it
// clone the slice and copy the changed entry. Clean output (the overwhelming
// common case — most events carry no secret) therefore allocates nothing and
// returns the original slice header unchanged.
//
// textutil.RedactSecrets is itself zero-alloc when no prefix matches (it
// aliases its input), so the per-field calls are cheap on the clean path.
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
