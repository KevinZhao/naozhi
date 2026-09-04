package textutil

import "strings"

// FirstLine returns the first non-empty line of s after TrimSpace, scanning
// past leading blank lines; "" if every line is blank. Used where an empty
// "title" would render as a blank chip. For the literal-first-line variant
// see [FirstLineLiteral].
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
		// No further newline: the tail is the last candidate line.
		if !strings.ContainsRune(s, '\n') {
			return strings.TrimSpace(s)
		}
	}
}

// FirstLineLiteral returns the literal first line of s — up to the first
// '\n', or all of s. Unlike [FirstLine], an empty first line is preserved
// (subagent transcript titles surface it intentionally).
func FirstLineLiteral(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
