package cli

import (
	"sync"
	"time"
	"unicode/utf8"
)

const defaultEventLogSize = 200

// EventEntry is a simplified event record for the dashboard.
type EventEntry struct {
	Time    int64   `json:"time"`              // unix ms
	Type    string  `json:"type"`              // init, thinking, tool_use, text, result, system
	Summary string  `json:"summary,omitempty"` // brief description
	Cost    float64 `json:"cost,omitempty"`    // cumulative cost (result events only)
}

// EventLog is a thread-safe, bounded event log.
type EventLog struct {
	mu      sync.RWMutex
	entries []EventEntry
	maxSize int
}

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]EventEntry, 0, 32)}
}

// Append adds an entry to the log, dropping oldest if full.
func (l *EventLog) Append(e EventEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Time == 0 {
		e.Time = time.Now().UnixMilli()
	}
	l.entries = append(l.entries, e)
	if len(l.entries) > l.maxSize {
		drop := l.maxSize / 4
		if drop < 1 {
			drop = 1
		}
		copy(l.entries, l.entries[drop:])
		l.entries = l.entries[:len(l.entries)-drop]
	}
}

// Entries returns a copy of all entries.
func (l *EventLog) Entries() []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]EventEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// EntriesSince returns entries after the given unix ms timestamp.
func (l *EventLog) EntriesSince(afterMS int64) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i, e := range l.entries {
		if e.Time > afterMS {
			out := make([]EventEntry, len(l.entries)-i)
			copy(out, l.entries[i:])
			return out
		}
	}
	return nil
}

// TruncateRunes truncates s to at most maxRunes runes, appending "..." if truncated.
// Uses byte-level rune decoding to avoid allocating a full []rune slice.
func TruncateRunes(s string, maxRunes int) string {
	i, count := 0, 0
	for i < len(s) {
		if count == maxRunes {
			return s[:i] + "..."
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s
}
