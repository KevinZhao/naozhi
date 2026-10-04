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
// lines are not kept so they cannot push the error text out. Lines are stored
// as written (only capped); consumers sanitize before logging or display. The
// zero value is ready to use and safe for concurrent use.
type StderrTail struct {
	mu    sync.Mutex
	lines []string
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
	for _, l := range lines {
		t.pushLocked(l)
	}
}

func (t *StderrTail) pushLocked(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	line = capStderrTailLine(line)
	if len(t.lines) < StderrTailLines {
		t.lines = append(t.lines, line)
		return
	}
	copy(t.lines, t.lines[1:])
	t.lines[len(t.lines)-1] = line
}

// Lines returns a copy of the kept lines, oldest first; nil when empty.
func (t *StderrTail) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return nil
	}
	return append([]string(nil), t.lines...)
}

func capStderrTailLine(line string) string {
	if len(line) <= StderrTailLineBytes {
		return line
	}
	cut := StderrTailLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}
