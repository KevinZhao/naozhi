package cli

import (
	"sync"
	"time"
	"unicode/utf8"
)

const defaultEventLogSize = 200

// EventEntry is a simplified event record for the dashboard.
type EventEntry struct {
	Time     int64   `json:"time"`               // unix ms
	Type     string  `json:"type"`               // init, thinking, tool_use, text, result, system
	Summary  string  `json:"summary,omitempty"`  // brief description
	Cost     float64 `json:"cost,omitempty"`     // cumulative cost (result events only)
	Detail   string  `json:"detail,omitempty"`   // fuller content for terminal view
	Tool     string  `json:"tool,omitempty"`     // tool name for tool_use events
	Subagent string  `json:"subagent,omitempty"` // subagent_type for Agent tool calls
}

type subscriber struct {
	ch        chan struct{} // buffered(1)
	closeOnce sync.Once
}

// EventLog is a thread-safe, bounded event log.
type EventLog struct {
	mu      sync.RWMutex
	entries []EventEntry
	maxSize int

	subMu       sync.Mutex
	subscribers []*subscriber
}

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]EventEntry, 0, 32)}
}

// Append adds an entry to the log, dropping oldest if full.
// Signals all subscribers non-blockingly after appending.
func (l *EventLog) Append(e EventEntry) {
	l.mu.Lock()
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
	l.mu.Unlock()

	l.subMu.Lock()
	for _, sub := range l.subscribers {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
	l.subMu.Unlock()
}

// Subscribe returns a notification channel and an unsubscribe function.
// The channel receives a signal (non-blocking) whenever Append is called.
func (l *EventLog) Subscribe() (<-chan struct{}, func()) {
	sub := &subscriber{ch: make(chan struct{}, 1)}
	l.subMu.Lock()
	l.subscribers = append(l.subscribers, sub)
	l.subMu.Unlock()

	unsub := func() {
		l.subMu.Lock()
		defer l.subMu.Unlock()
		for i, s := range l.subscribers {
			if s == sub {
				l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
				break
			}
		}
		sub.closeOnce.Do(func() { close(sub.ch) })
	}
	return sub.ch, unsub
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
