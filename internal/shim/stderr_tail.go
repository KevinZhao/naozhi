package shim

import (
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// StderrTailLines is how many of the CLI's most recent stderr lines a
	// StderrTail keeps; the cause of a startup failure is in the last few.
	StderrTailLines = 8
	// StderrTailLineBytes caps one kept line (cut on a rune boundary).
	StderrTailLineBytes = 512
)

// StderrTail is the bounded tail of a CLI's stderr, oldest line first. Blank
// lines are not kept so they cannot push the error text out, and the first
// error line (IsStderrErrorLine) stays as the oldest line once a long stack
// trace scrolls it out. Lines are stored as written (only capped); consumers
// sanitize before logging or display. The zero value is ready to use and safe
// for concurrent use.
type StderrTail struct {
	mu    sync.Mutex
	lines []string
	// pushed counts kept lines; cause is the first error line and causeSeq
	// its count, so it is still in lines while causeSeq >= pushed-len(lines).
	pushed   int
	cause    string
	causeSeq int
}

// Push appends line, dropping the oldest once StderrTailLines are kept.
func (t *StderrTail) Push(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pushLocked(line)
}

// Replace discards the kept lines and pushes lines in order.
func (t *StderrTail) Replace(lines []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = t.lines[:0]
	t.pushed, t.cause = 0, ""
	for _, l := range lines {
		t.pushLocked(l)
	}
}

func (t *StderrTail) pushLocked(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	line = capStderrTailLine(line)
	if t.cause == "" && IsStderrErrorLine(line) {
		t.cause, t.causeSeq = line, t.pushed
	}
	t.pushed++
	if len(t.lines) < StderrTailLines {
		t.lines = append(t.lines, line)
		return
	}
	copy(t.lines, t.lines[1:])
	t.lines[len(t.lines)-1] = line
}

// Lines returns a copy of the kept lines, oldest first; nil when empty. When
// the first error line has scrolled out it replaces the oldest kept line.
func (t *StderrTail) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return nil
	}
	if t.cause != "" && t.causeSeq < t.pushed-len(t.lines) {
		return append([]string{t.cause}, t.lines[1:]...)
	}
	return append([]string(nil), t.lines...)
}

// IsStderrErrorLine reports whether line starts with an error label such as
// "Error:", "TypeError:", "Error [ERR_X]:" or bun's "error:", which is how
// node, bun and the claude CLI print the cause of a failure.
func IsStderrErrorLine(line string) bool {
	t := strings.TrimSpace(line)
	i := strings.IndexByte(t, ':')
	if i <= 0 {
		return false
	}
	label := t[:i]
	if j := strings.Index(label, " ["); j > 0 && strings.HasSuffix(label, "]") {
		label = label[:j]
	}
	if label != "error" && !strings.HasSuffix(label, "Error") {
		return false
	}
	for _, c := range label {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func capStderrTailLine(line string) string {
	if len(line) <= StderrTailLineBytes {
		return line
	}
	cut := StderrTailLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	// Clone so a kept prefix does not pin the whole line (up to the
	// scanner's 10 MiB) for the life of the shim.
	return strings.Clone(line[:cut])
}
