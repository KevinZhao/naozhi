package history

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Canonical truncation caps for entries surfaced from external-CLI
// transcripts (claude history_tail, kirojsonl, codexjsonl). Summary is the
// one-line preview; Detail is the verbatim quoted text. Without the Detail
// cap the full message (up to each reader's per-line scanner limit) flows
// verbatim across the WS boundary and the dashboard renders an unbounded
// mega-bubble.
//
// Note the live tier caps some branches tighter (user prompts, thinking
// and tool details use cli.EventDetailMaxRunes, 2000; assistant text also
// uses 16000) — merged.contentKey normalises to the tightest cap when
// pairing entries across tiers, so these bounds may differ safely.
const (
	SummaryMaxRunes = 120
	DetailMaxRunes  = 16000
)

// NewDerivedEntry builds the canonical cli.EventEntry for an external-CLI
// transcript line from its (timestamp, type, full text) triple. It is THE
// single derivation recipe shared by every fallback history source:
// truncate fullText to the (SummaryMaxRunes, DetailMaxRunes) caps, then
// derive a deterministic UUID over the truncated pair.
//
// The deterministic UUID is what lets merged.Source dedup overlapping
// LoadBefore pages: with an empty UUID merged.mergeSorted treats the entry
// as un-dedupable and the same transcript line renders twice whenever a
// `beforeMS` cursor straddles a previously-returned entry. The real detail
// (not "") is folded into the hash so two lines sharing the same timestamp
// and 120-rune summary but differing in the detail tail derive distinct
// UUIDs (#2336) — this recipe used to be copy-pasted per source and the
// codex copy silently drifted to detail="", which is why it now lives here.
func NewDerivedEntry(timeMS int64, entryType, fullText string) cli.EventEntry {
	summary, detail := textutil.TruncateRunesPair(fullText, SummaryMaxRunes, DetailMaxRunes)
	return cli.EventEntry{
		UUID:    textutil.DeriveLegacyUUID(timeMS, entryType, summary, detail),
		Time:    timeMS,
		Type:    entryType,
		Summary: summary,
		Detail:  detail,
	}
}
