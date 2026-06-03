package osutil

import "strings"

// HasNoPathTrigger reports whether s contains none of the three bytes that can
// begin a redactable path token: a POSIX slash, a Windows backslash, or a
// tilde-home shorthand. Callers use it as a cheap pre-check to skip the scan
// (and any Builder allocation) for path-free strings.
//
// R249-ARCH-17 (#983): promoted from internal/cron so other daemons
// (sysession etc.) can reuse the same redaction trigger set without
// duplicating the byte scan or risking drift between copies.
func HasNoPathTrigger(s string) bool {
	return strings.IndexByte(s, '/') < 0 &&
		strings.IndexByte(s, '\\') < 0 &&
		strings.IndexByte(s, '~') < 0
}

// RedactAbsolutePathsInto scans s for absolute filesystem paths and writes the
// redacted result into b (each path token replaced by the literal "<path>").
// Writing into a caller-provided Builder lets hot-path callers (cron's
// per-run error sanitiser) reuse a pooled Builder so the only per-call
// allocation is the final String() copy.
//
// Detection covers three forms — POSIX `/abs`, Windows drive `C:\…` / `C:/…`,
// and home-relative `~/`. A bare root ("/", "/ ", "/\n") is treated as a
// literal byte because it carries no per-host/per-user information. UNC paths
// (`\\server\share`) are intentionally out of scope.
//
// R249-ARCH-17 (#983): promoted verbatim from internal/cron
// redactPathsInCronError's inner scan so the path-redaction policy lives in
// one cross-cutting place. The cron wrapper keeps its truncation + Builder
// pool and delegates the scan here.
func RedactAbsolutePathsInto(b *strings.Builder, s string) {
	i := 0
	for i < len(s) {
		c := s[i]
		// POSIX absolute path: leading '/' followed by a non-space/non-quote
		// byte. A lone '/' (end-of-string or before whitespace) is a bare-root
		// reference, never a sensitive path, so it falls through as a literal.
		isPosix := c == '/' && i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\t' && s[i+1] != '\n'
		isWin := i+2 < len(s) &&
			((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) &&
			s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/')
		// home-relative "~/" — only when preceded by a separator or line start
		// so "weight ~5kg" style text is not mis-redacted.
		isTildeHome := c == '~' && i+1 < len(s) && s[i+1] == '/' &&
			(i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' ||
				s[i-1] == '\'' || s[i-1] == '"' || s[i-1] == '`' ||
				s[i-1] == ',' || s[i-1] == ';' || s[i-1] == '(' || s[i-1] == '=')
		if !isPosix && !isWin && !isTildeHome {
			b.WriteByte(c)
			i++
			continue
		}
		// Consume the path until a delimiter that cannot appear in a typical
		// error-embedded path. Stopping at whitespace is the key rule:
		// std-lib errors spell paths as whitespace-separated tokens
		// ("open /tmp/x: reason"). A conservative scan over-redacts on the
		// rare path-with-space rather than leaking.
		j := i
		for j < len(s) {
			cc := s[j]
			if cc == '\n' || cc == ' ' || cc == '\t' || cc == ',' || cc == ';' ||
				cc == '\'' || cc == '"' || cc == '`' {
				break
			}
			if cc == ':' && j+1 < len(s) && (s[j+1] == ' ' || s[j+1] == '\n') {
				// `path: reason` — stop before the ':' so the reason tail
				// survives redaction.
				break
			}
			j++
		}
		b.WriteString("<path>")
		i = j
	}
}

// RedactAbsolutePaths is the convenience string→string form of
// RedactAbsolutePathsInto for callers without a pooled Builder. Returns s
// unchanged (no allocation) when it contains no path-trigger byte.
func RedactAbsolutePaths(s string) string {
	if s == "" || HasNoPathTrigger(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	RedactAbsolutePathsInto(&b, s)
	return b.String()
}
