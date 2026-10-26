package server

import (
	"testing"
	"time"
)

// TestDispatchHealthProbe_PopulatesAndOmitsLastReply pins both arms of
// the dispatch probe: counters always copy through, and the
// last-reply-success fields are emitted only when the timestamp is
// non-zero (byte-identical to the prior inline `if !lastReply.IsZero()`
// guard). #1052.
func TestDispatchHealthProbe_PopulatesAndOmitsLastReply(t *testing.T) {
	// Zero lastReply → counters set, timestamp fields stay empty.
	auth := &healthAuthSection{}
	dispatchHealthProbe(func() (int64, int64, int64, time.Time) {
		return 3, 1, 2, time.Time{}
	})(auth)
	if auth.Dispatch == nil {
		t.Fatal("Dispatch must be non-nil after probe with live closure")
	}
	if auth.Dispatch.MessageCount != 3 || auth.Dispatch.ReplyErrorCount != 1 || auth.Dispatch.SendFailCount != 2 {
		t.Errorf("dispatch counters = %+v, want {3,1,2}", auth.Dispatch)
	}
	if auth.Dispatch.LastReplySuccessAt != "" || auth.Dispatch.LastReplySuccessAgo != "" {
		t.Errorf("last-reply fields must stay empty for zero timestamp, got at=%q ago=%q",
			auth.Dispatch.LastReplySuccessAt, auth.Dispatch.LastReplySuccessAgo)
	}

	// Non-zero lastReply → RFC3339 + ago populated.
	auth2 := &healthAuthSection{}
	dispatchHealthProbe(func() (int64, int64, int64, time.Time) {
		return 0, 0, 0, time.Now().Add(-time.Minute)
	})(auth2)
	if auth2.Dispatch.LastReplySuccessAt == "" || auth2.Dispatch.LastReplySuccessAgo == "" {
		t.Errorf("last-reply fields must be set for non-zero timestamp, got at=%q ago=%q",
			auth2.Dispatch.LastReplySuccessAt, auth2.Dispatch.LastReplySuccessAgo)
	}
}
