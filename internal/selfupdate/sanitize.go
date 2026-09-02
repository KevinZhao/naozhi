// sanitize.go — scrubbing for error strings that cross the process boundary
// into a browser.
//
// Everything in the Status object is reachable from `GET /api/system/update`,
// which means download and restart errors get rendered as HTML by whatever is
// logged into the dashboard. Two things in those errors are worth removing
// before they leave the process:
//
//  1. URL query strings. GitHub redirects release-asset downloads to
//     objects.githubusercontent.com with pre-signed credentials in the query
//     (`?X-Amz-Signature=…&X-Amz-Credential=…`). A wrapped *url.Error carries
//     the full URL, so a plain download failure would otherwise paste a
//     time-limited signed URL into the page.
//  2. Anything RedactSecrets already knows about — env assignments and
//     well-known token prefixes — since a failing subprocess can echo its own
//     environment into CombinedOutput.
//
// Local filesystem paths are deliberately NOT scrubbed: the audience for
// these messages is the operator, and "which directory was not writable" is
// the single most useful thing the message can say.
package selfupdate

import (
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/textutil"
)

// maxErrRunes bounds a sanitized error. systemctl/launchctl failures embed
// CombinedOutput, which can run long; the dashboard only has room for a line
// or two and the full text is in the log either way.
const maxErrRunes = 400

// urlQueryRe matches the query portion of an http(s) URL. Anchoring on the
// scheme keeps it from eating a bare `?` in prose. The terminator set stops at
// whitespace and at the quote/paren characters Go's error wrapping tends to
// put around URLs, so only the query is dropped and surrounding text survives.
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
	// Strip signed-URL queries first: RedactSecrets works on `KEY=value`
	// shapes and would leave `X-Amz-Signature=…` inside a query it does not
	// recognize as an env assignment.
	s = urlQueryRe.ReplaceAllString(s, "$1?…")
	s = textutil.RedactSecrets(s)
	// Collapse newlines — CombinedOutput is multi-line and the dashboard
	// renders this as a single line of text.
	s = strings.Join(strings.Fields(s), " ")
	return textutil.TruncateRunes(s, maxErrRunes)
}
