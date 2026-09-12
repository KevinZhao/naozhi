// Package replyfmt holds the reply-shaping rules shared by the two paths that
// deliver agent output to a chat: internal/dispatch (IM replies) and
// internal/cron (scheduled-job notifications). J2 of #2548.
//
// # Why a package and not a helper in one of them
//
// internal/cron must NOT import internal/platform — #725, pinned by
// no_platform_import_test.go — so it reaches its platforms through the narrow
// PlatformReplier adapter. That rules out the shared entry point #2548 sketched
// (`SplitReply(ctx, platform.Platform, …)`); anything both sides can call has to
// be free of platform. This package imports internal/textutil and nothing else.
//
// # What is here and what is deliberately not
//
// Here: the formatting decisions, which were byte-identical duplicates or missing
// on one side.
//
//   - TruncateForSingleReply existed twice, character for character, because cron
//     could not import dispatch either.
//   - PageSuffix and ReserveForPageSuffix existed only in dispatch, which is the
//     user-visible half of this: a cron notification split across four messages
//     arrived with no "[2/4]", so a recipient could not tell whether they had the
//     whole thing or which part they were reading.
//
// Not here: the send loops. They differ on purpose and merging them would produce
// one function with five policy flags —
//
//	                        dispatch          cron
//	chunk-count cap         none              cronNotifyMaxChunks (#568)
//	per-chunk deadline      none              replyCtx.Err() (#799)
//	on chunk failure        continue          abort (#1151)
//	failure metric          sendFailCount     CronNotifyPartialTotal (#966)
//	chatID in logs          verbatim          SanitizeForLog (attacker-influenced)
//
// Every one of those rows has an issue behind it. A shared loop would have to
// re-express all five as options, which is the shape that made the difference hard
// to see in the first place.
package replyfmt

import (
	"strconv"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/textutil"
)

// SingleReplyTruncMarker is appended, within the rune budget, when a reply to a
// single-use-token platform (WeChat iLink and friends: one message per inbound
// turn) has to be collapsed into one message. Visible on purpose — a silent
// truncation reads as the agent stopping mid-sentence (#2136, #2181).
const SingleReplyTruncMarker = "\n…(truncated)"

// singleReplyTruncMarkerRunes is the marker's rune width.
var singleReplyTruncMarkerRunes = utf8.RuneCountInString(SingleReplyTruncMarker)

// TruncateForSingleReply trims text to at most maxRunes runes, reserving room for
// the marker; when maxRunes cannot fit the marker it falls back to bare rune-safe
// truncation.
func TruncateForSingleReply(text string, maxRunes int) string {
	keep := maxRunes - singleReplyTruncMarkerRunes
	if keep <= 0 {
		// No room for the marker — keep as much content as fits.
		return textutil.TruncateRunesNoEllipsis(text, maxRunes)
	}
	return textutil.TruncateRunesNoEllipsis(text, keep) + SingleReplyTruncMarker
}

// PageSuffix is the "\n— [i/total]" tail marking chunk i of total. 1-based.
func PageSuffix(i, total int) string {
	return "\n— [" + strconv.Itoa(i) + "/" + strconv.Itoa(total) + "]"
}

// pageSuffixRuneWidth returns the rune width of the worst-case page suffix
// "\n— [i/total]": 6 fixed runes ('\n' '—' ' ' '[' '/' ']') plus two numbers
// with total's digit count (#2008).
func pageSuffixRuneWidth(total int) int {
	if total < 1 {
		total = 1
	}
	digits := len(strconv.Itoa(total))
	return 6 + 2*digits
}

// upperBoundChunks returns a ceiling on how many chunks SplitText produces for
// runeCount runes at splitWidth; it must never under-estimate because the
// caller reserves the page-suffix budget from it. SplitText may break early at
// a newline past the chunk midpoint, so the shortest chunk is ~ceil(splitWidth/2)
// — a naive ceil(runeCount/splitWidth) can under-estimate by 2x (#2056).
func upperBoundChunks(runeCount, splitWidth int) int {
	if splitWidth <= 0 {
		return runeCount + 1
	}
	minChunk := (splitWidth + 1) / 2 // ceil(splitWidth/2), worst-case shortest chunk
	if minChunk < 1 {
		minChunk = 1
	}
	return (runeCount + minChunk - 1) / minChunk
}

// ReserveForPageSuffix returns the width to split at so that a chunk plus its
// "[i/N]" suffix still fits maxLen, and whether the suffix must be suppressed
// entirely.
//
// Splitting at the raw limit and appending the suffix afterwards pushes full
// chunks past hard API ceilings — Discord rejects >2000 outright and the retry
// re-sends the same oversized payload (#2008). The reservation is computed in two
// passes because the suffix width depends on the chunk count, which depends on the
// width: assume one digit, then widen to the worst case for the resulting count.
// Over-reserving is safe; under-reserving is not.
//
// suppress is true when maxLen cannot fit even the suffix (reachable because
// config only clamps maxLen <= 0): emitting guaranteed-oversized chunks would be
// worse than dropping the marker (#2057).
func ReserveForPageSuffix(maxLen, runeCount int) (splitLen int, suppress bool) {
	if runeCount <= maxLen {
		return maxLen, false
	}
	reserved := maxLen - pageSuffixRuneWidth(upperBoundChunks(runeCount, maxLen-pageSuffixRuneWidth(1)))
	if reserved > 0 {
		return reserved, false
	}
	return maxLen, true
}
