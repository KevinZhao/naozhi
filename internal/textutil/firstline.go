package textutil

import "strings"

// FirstLine returns the first non-empty line of s after TrimSpace, scanning
// past any leading blank lines. Used by status banners / cron title fallback
// where rendering an empty first line as the "title" would produce a blank
// chip in the UI.
//
// Semantics (R222-CR-5 / R222-CR-7):
//   - TrimSpace the whole input first.
//   - If no newline, return the trimmed input.
//   - Otherwise scan forward over '\n'-separated lines, returning the first
//     line whose TrimSpace is non-empty. Returns "" if every line is blank.
//
// This unifies the logic that previously lived in dispatch/status.go::firstLine
// (looked at first + second lines only) and cron/job.go::JobTitleOrFallback
// (scanned all lines via strings.Split). The "scan all lines" behaviour is
// the strict superset, so callers that previously stopped at the second line
// still get correct output for inputs whose first two lines are non-empty.
//
// For the literal-first-line variant (preserves an empty first line, used by
// subagent transcript title extraction), see [FirstLineLiteral].
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			return s
		}
		first := strings.TrimSpace(s[:idx])
		if first != "" {
			return first
		}
		s = s[idx+1:]
		// strings.TrimSpace at the loop top is overkill on every iteration;
		// the next IndexByte + TrimSpace already collapses leading whitespace
		// for the upcoming candidate line, and a final all-whitespace tail
		// returns "" via the IndexByte<0 path's TrimSpace at function entry
		// only — so guard the no-newline-tail path explicitly.
		if !strings.ContainsRune(s, '\n') {
			return strings.TrimSpace(s)
		}
	}
}

// FirstLineLiteral returns the literal first line of s — the slice up to the
// first '\n', or all of s if no newline exists. Unlike [FirstLine], an empty
// first line is preserved (returns ""); callers in subagent transcripts
// intentionally surface this so a transcript whose first row has no content
// renders as empty rather than silently scanning ahead for the next non-blank
// row. R222-CR-5.
func FirstLineLiteral(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
