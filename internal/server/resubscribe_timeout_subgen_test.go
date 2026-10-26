package server

import "testing"

// TestResubscribeTimeout_SchedulesGenerationReclaim: the resubscribe timeout
// ends a subscription through expire, which schedules the key's generation
// for reclamation exactly as unsubscribe does. Without that, a client that
// keeps subscribing to panels whose process is gone would pin one generation
// per key for the whole connection.
func TestResubscribeTimeout_SchedulesGenerationReclaim(t *testing.T) {
	r := newSubscriberRegistry()
	c := &wsClient{done: make(chan struct{})}
	r.add(c)
	r.reserve(c, "k")
	r.install(c, "k", func() {}, func() bool { return true })

	const now = int64(7_000_000_000)
	if stale := r.expire(c, "k", now); stale == nil {
		t.Fatal("expire did not hand back the closure")
	}
	cs := r.clients[c]
	if got, ok := cs.releaseAt["k"]; !ok || got != now+subGenRetentionNanos {
		t.Errorf("expire did not schedule reclamation (releaseAt=%v, ok=%v)", got, ok)
	}
	if _, ok := cs.unsubs["k"]; ok {
		t.Error("expire left the subscription in place")
	}
	if again := r.expire(c, "k", now); again != nil {
		t.Error("a second expire handed back a closure")
	}
}
