package cli

import (
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const defaultEventLogSize = 500

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

// EventLog is a thread-safe, bounded event log backed by a ring buffer.
type EventLog struct {
	mu      sync.RWMutex
	entries []EventEntry // ring buffer, pre-allocated to maxSize
	head    int          // next write position
	count   int          // number of valid entries (0..maxSize)
	maxSize int

	// Cached summaries updated atomically on Append for efficient access
	// without copying all entries.
	lastPromptSummary   atomic.Value // string: most recent "user" entry summary
	lastActivitySummary atomic.Value // string: most recent "tool_use"/"thinking" entry summary

	subMu       sync.Mutex
	subscribers []*subscriber
}

// NewEventLog creates an event log with the given max size.
func NewEventLog(maxSize int) *EventLog {
	if maxSize <= 0 {
		maxSize = defaultEventLogSize
	}
	return &EventLog{maxSize: maxSize, entries: make([]EventEntry, maxSize)}
}

// Append adds an entry to the log, overwriting the oldest entry when full.
// Signals all subscribers non-blockingly after appending.
func (l *EventLog) Append(e EventEntry) {
	l.mu.Lock()
	if e.Time == 0 {
		e.Time = time.Now().UnixMilli()
	}
	l.entries[l.head] = e
	l.head = (l.head + 1) % l.maxSize
	if l.count < l.maxSize {
		l.count++
	}
	l.mu.Unlock()

	// Update cached summaries (atomic, no lock needed).
	switch e.Type {
	case "user":
		l.lastPromptSummary.Store(e.Summary)
	case "tool_use", "thinking", "agent":
		l.lastActivitySummary.Store(e.Summary)
	}

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

// Entries returns a copy of all entries in chronological order.
func (l *EventLog) Entries() []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]EventEntry, l.count)
	start := (l.head - l.count + l.maxSize) % l.maxSize
	for i := 0; i < l.count; i++ {
		out[i] = l.entries[(start+i)%l.maxSize]
	}
	return out
}

// EntriesSince returns entries after the given unix ms timestamp, in chronological order.
func (l *EventLog) EntriesSince(afterMS int64) []EventEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	start := (l.head - l.count + l.maxSize) % l.maxSize
	for i := 0; i < l.count; i++ {
		idx := (start + i) % l.maxSize
		if l.entries[idx].Time > afterMS {
			remaining := l.count - i
			out := make([]EventEntry, remaining)
			for j := 0; j < remaining; j++ {
				out[j] = l.entries[(idx+j)%l.maxSize]
			}
			return out
		}
	}
	return nil
}

// LastPromptSummary returns the summary of the most recent "user" entry.
func (l *EventLog) LastPromptSummary() string {
	if v := l.lastPromptSummary.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// LastActivitySummary returns the summary of the most recent "tool_use" or "thinking" entry.
func (l *EventLog) LastActivitySummary() string {
	if v := l.lastActivitySummary.Load(); v != nil {
		return v.(string)
	}
	return ""
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
