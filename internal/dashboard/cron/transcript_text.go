package cron

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// ansiEscRe matches common ANSI CSI sequences (color, cursor motion) AND OSC
// sequences (e.g. hyperlink `\x1b]8;;url\x1b\\` / BEL-terminated `\x1b]8;;url\x07`,
// emitted by gh / ls --hyperlink). Stripped from tool output before
// serialising so the rendered <pre> doesn't show garbled escape codes; the
// dashboard's esc()-then-<pre> already prevents HTML interpretation (#788).
var ansiEscRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// parseRunPathParams extracts run_id (path) + job_id (query) and
// validates both. Centralised so HandleRunDetail and HandleRunTranscript
// share the exact same gate.
func parseRunPathParams(w http.ResponseWriter, r *http.Request) (runID, jobID string, ok bool) {
	runID = r.PathValue("run_id")
	if runID == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "run_id is required"})
		return "", "", false
	}
	if len(runID) > runIDLenLimit {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "run_id too long"})
		return "", "", false
	}
	if !cronpkg.IsValidID(runID) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "run_id must be lowercase hex"})
		return "", "", false
	}
	jobID = r.URL.Query().Get("job_id")
	if jobID == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "job_id is required"})
		return "", "", false
	}
	if len(jobID) > maxCronIDLenDashboard {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "job_id too long"})
		return "", "", false
	}
	if !cronpkg.IsValidID(jobID) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "job_id must be lowercase hex"})
		return "", "", false
	}
	return runID, jobID, true
}

// sanitizeWireText drops bidi / C1 / LS-PS runes (the IsLogInjectionRune
// class) AND C0 control bytes (except \t / \n / \r) before transcript turn
// fields reach the JSON wire, then redacts secrets. Preserves \t / \n / \r so
// multi-line tool_result rendering survives — SanitizeForLog would map those
// to '_' and destroy formatting in the dashboard's <pre> sink.
//
// Defence-in-depth: bidi overrides in a JSONL file could otherwise corrupt
// visual ordering despite esc()-then-<pre>, and 0x1B ESC would trigger ANSI
// interpretation when operators paste transcript JSON into a terminal (#1331).
func sanitizeWireText(s string) string {
	if s == "" {
		return s
	}
	// Fast path: drop nothing if string is pure ASCII printable (with the
	// three preserved whitespace runes). Any C0 control byte (< 0x20) other
	// than \t/\n/\r forces the slow path even on pure ASCII; bidi / C1
	// codepoints encode with leading byte ≥ 0x80 in UTF-8.
	dirty := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x80 {
			dirty = true
			break
		}
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			dirty = true
			break
		}
	}
	if !dirty {
		return textutil.RedactSecrets(s)
	}
	cleaned := strings.Map(func(r rune) rune {
		// Drop C0 control runes (incl. 0x1B ESC) except \t / \n / \r.
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		if osutil.IsLogInjectionRune(r) {
			return -1 // drop
		}
		return r
	}, s)
	return textutil.RedactSecrets(cleaned)
}

// truncateRunes caps a string to maxBytes by rune boundary, appending "…"
// (3 bytes) when truncation happened. Cutting by rune rather than byte keeps
// multi-byte UTF-8 sequences intact (a split would render as U+FFFD). The
// walk stops when adding the next rune would push the final length (after
// the "…" suffix) past maxBytes, so the result is never over the cap.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…" // 3 bytes UTF-8.
	// Honour the cap even when maxBytes is too small to fit the ellipsis —
	// without this the function would return the bare ellipsis (3 bytes) on
	// any maxBytes < 3 input, violating the "caps to maxBytes" contract.
	if maxBytes < len(ellipsis) {
		return ""
	}
	budget := maxBytes - len(ellipsis)
	// cut tracks the byte offset where we may safely cut: the end of the
	// last rune we have committed. Iterating with range gives us the start
	// byte index of each rune; we commit a rune when its end position fits
	// the budget.
	cut := 0
	for i, r := range s {
		size := utf8.RuneLen(r)
		if size < 0 {
			size = len(string(utf8.RuneError))
		}
		if i+size > budget {
			break
		}
		cut = i + size
	}
	return s[:cut] + ellipsis
}
