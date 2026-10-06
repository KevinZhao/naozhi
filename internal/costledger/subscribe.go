package costledger

import "time"

// Subscribe calls fn with every durable entry whose TS is at or after from:
// first those already on disk, then each new one once its batch is written
// (within about flushEvery of Append). The replay holds the writer, so no
// entry is delivered twice or missed; call it at startup, as a long replay
// backs up the queue. fn runs on the writer goroutine: it must not block or
// keep e.Models. A disabled store never calls fn; a read-only or closed one
// replays only.
func (s *Store) Subscribe(from time.Time, fn func(Entry)) {
	if s == nil || s.disabled || fn == nil {
		return
	}
	from = from.UTC()
	s.subMu.Lock()
	defer s.subMu.Unlock()
	first := from.Format(dayLayout)
	for _, d := range s.dayFiles() {
		if d < first {
			continue
		}
		s.scanDay(d, func(e Entry) bool {
			if !e.TS.Before(from) {
				fn(e)
			}
			return true
		})
	}
	if s.replayedHook != nil {
		s.replayedHook()
	}
	if !s.closed.Load() {
		s.subs = append(s.subs, fn)
	}
}
