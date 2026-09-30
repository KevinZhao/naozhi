package session

import (
	"testing"
	"time"
)

// A retirement always invalidates the history cache, even one with no session
// UUID or no store to stamp it in: the key still left the live list.
func TestRecordRetired_AlwaysInvalidatesHistoryCache(t *testing.T) {
	for _, sid := range []string{"", "sid-1"} {
		h := &Handlers{}
		h.historyCacheTime = time.Now()
		h.historyCacheTimeUnixNano.Store(h.historyCacheTime.UnixNano())
		h.RecordRetired(sid)
		if !h.historyCacheTime.IsZero() || h.historyCacheTimeUnixNano.Load() != 0 {
			t.Errorf("RecordRetired(%q) left the history cache fresh", sid)
		}
	}
}
