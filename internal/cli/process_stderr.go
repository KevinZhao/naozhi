package cli

// process_stderr.go — the CLI's stderr tail as naozhi keeps it: live stderr
// frames, replaced by the shim's own tail when cli_exited carries one.

import (
	"strings"

	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/textutil"
)

// stderrSummaryRunes caps the one-line stderr cause quoted in error text.
const stderrSummaryRunes = 200

// StderrTail returns the CLI's last stderr lines, oldest first, sanitized and
// secret-redacted; nil when the CLI wrote none.
func (p *Process) StderrTail() []string { return p.stderrTail.Lines() }

// recordStderrLine keeps one live stderr line; line is already sanitized.
func (p *Process) recordStderrLine(line string) {
	p.stderrTail.Push(textutil.RedactSecrets(line))
}

// adoptExitStderrTail takes the tail a cli_exited frame carries and returns
// the tail to report. The shim's tail wins because it also holds lines written
// before naozhi attached; a frame without one (an older shim) keeps the tail
// built from stderr frames. The shim is a separate process, so its lines are
// sanitized here like any other stderr.
func (p *Process) adoptExitStderrTail(frameTail []string) []string {
	if len(frameTail) > 0 {
		frameTail = frameTail[max(0, len(frameTail)-shim.StderrTailLines):]
		clean := make([]string, len(frameTail))
		for i, l := range frameTail {
			clean[i] = textutil.RedactSecrets(sanitizeStderrLine(l))
		}
		p.stderrTail.Replace(clean)
	}
	return p.stderrTail.Lines()
}

// stderrTailSummary is the first error line of tail (shim.IsStderrErrorLine),
// else its first line that is not a warning, else its first line, capped at
// stderrSummaryRunes for one-line error text; "" when tail is empty.
func stderrTailSummary(tail []string) string {
	if len(tail) == 0 {
		return ""
	}
	line := ""
	for _, l := range tail {
		if shim.IsStderrErrorLine(l) {
			line = l
			break
		}
		if line == "" && !isStderrWarningLine(l) {
			line = l
		}
	}
	if line == "" {
		line = tail[0]
	}
	return textutil.TruncateRunes(line, stderrSummaryRunes)
}

// isStderrWarningLine reports whether line is a warning the CLI or node
// printed and ran on past, e.g. "claude: warning: failed to merge user
// --settings: ..." or node's "(node:42) DeprecationWarning: ...".
func isStderrWarningLine(line string) bool {
	return strings.Contains(strings.ToLower(line), "warning:")
}
