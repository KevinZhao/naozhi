package history

import (
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Truncation caps for entries derived from external-CLI transcripts. Summary
// is the one-line preview; Detail is the quoted text. The live tier caps some
// branches tighter (cli.EventDetailMaxRunes); merged.contentKey normalises to
// the tightest cap when pairing across tiers.
const (
	SummaryMaxRunes = 120
	DetailMaxRunes  = 16000
)

// NewDerivedEntry builds the canonical clievent.EventEntry for an external-CLI
// transcript line: truncate fullText to the Summary/Detail caps, then derive a
// deterministic UUID over the truncated pair. That UUID is what lets
// merged.Source dedup overlapping LoadBefore pages; the detail is folded into
// the hash so two lines sharing a timestamp and summary but differing in the
// detail tail get distinct UUIDs (#2336).
func NewDerivedEntry(timeMS int64, entryType, fullText string) clievent.EventEntry {
	summary, detail := textutil.TruncateRunesPair(fullText, SummaryMaxRunes, DetailMaxRunes)
	return clievent.EventEntry{
		UUID:    textutil.DeriveLegacyUUID(timeMS, entryType, summary, detail),
		Time:    timeMS,
		Type:    entryType,
		Summary: summary,
		Detail:  detail,
	}
}
