// sanitize.go — scrubbing for error strings that reach the dashboard via
// `GET /api/system/update`: URL query strings (GitHub asset redirects carry
// pre-signed `?X-Amz-Signature=…` credentials inside *url.Error) and anything
// RedactSecrets knows about (a failing subprocess can echo its env). Local
// paths are deliberately kept — "which dir was not writable" is the point.
package selfupdate

import (
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/textutil"
)

// maxErrRunes bounds a sanitized error; the full text is in the log anyway.
const maxErrRunes = 400

// urlQueryRe matches the query of an http(s) URL, anchored on the scheme so a
// bare `?` in prose survives and terminated at whitespace/quotes/parens.
var urlQueryRe = regexp.MustCompile(`(https?://[^\s"'\)]*?)\?[^\s"'\)]*`)

// sanitizeErr renders err for dashboard consumption. Returns "" for a nil
// error so callers can pass through unconditionally.
func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrString(err.Error())
}

// sanitizeErrString is the string-level half, split out so tests can exercise
// the scrubbing without constructing errors.
func sanitizeErrString(s string) string {
	if s == "" {
		return ""
	}
	// Queries first: RedactSecrets would not recognise `X-Amz-Signature=…`
	// inside a query as an env assignment.
	s = urlQueryRe.ReplaceAllString(s, "$1?…")
	s = textutil.RedactSecrets(s)
	// CombinedOutput is multi-line; the dashboard renders one line.
	s = strings.Join(strings.Fields(s), " ")
	return textutil.TruncateRunes(s, maxErrRunes)
}
