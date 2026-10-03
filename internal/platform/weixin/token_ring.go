package weixin

import "sync"

// tokenRingCap bounds the context_tokens kept per user. Two covers the
// collect-mode case (answer + merged follow-up); the slack absorbs a burst.
const tokenRingCap = 4

// tokenRing holds a user's most recent inbound context_tokens, one slot per
// distinct token. iLink accepts each token for one send, so a slot is spent
// once a send on it may have been accepted (see #3003).
type tokenRing struct {
	mu        sync.Mutex
	slots     []tokenSlot // oldest first, len <= tokenRingCap
	updatedNs int64       // stamp of the newest push; drives TTL eviction
	evicted   bool        // set by evictIfIdle; a pusher must start a new ring
}

type tokenSlot struct {
	token string
	spent bool
	// uncertain marks an unspent token whose last send got no verdict: that
	// send may have been delivered. Only meaningful while !spent.
	uncertain bool
}

// lease is one send attempt's token. reserved is false for the last-resort
// reuse of a spent token; uncertain is copied from the slot at take time.
type lease struct {
	token     string
	reserved  bool
	uncertain bool
}

// push records an inbound token as the newest slot, dropping the oldest past
// tokenRingCap. A redelivered token keeps its slot and state. Returns false
// when the ring was evicted concurrently.
func (r *tokenRing) push(token string, nowNs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.evicted {
		return false
	}
	r.updatedNs = nowNs
	for _, s := range r.slots {
		if s.token == token {
			return true
		}
	}
	if len(r.slots) == tokenRingCap {
		r.slots = append(r.slots[:0], r.slots[1:]...)
	}
	r.slots = append(r.slots, tokenSlot{token: token})
	return true
}

// evictIfIdle marks the ring dead when its newest push is older than cutoffNs.
func (r *tokenRing) evictIfIdle(cutoffNs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updatedNs < cutoffNs {
		r.evicted = true
	}
	return r.evicted
}

// take returns the token for the next send attempt, reserved (marked spent)
// so a concurrent Reply picks another: an uncertain token first, so a retry
// stays on the token its earlier send may have spent, else the newest unspent
// one. With nothing unspent it returns the newest token unreserved as the last
// resort. ok is false for an empty ring.
func (r *tokenRing) take() (l lease, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pick := -1
	for i := len(r.slots) - 1; i >= 0; i-- {
		if s := r.slots[i]; !s.spent && (s.uncertain || pick < 0) {
			pick = i
			if s.uncertain {
				break
			}
		}
	}
	if pick >= 0 {
		s := &r.slots[pick]
		l = lease{token: s.token, reserved: true, uncertain: s.uncertain}
		s.spent = true
		return l, true
	}
	if n := len(r.slots); n > 0 {
		return lease{token: r.slots[n-1].token}, true
	}
	return lease{}, false
}

// release hands a reserved token back unspent. uncertain records that the
// send got no verdict and may have been delivered.
func (r *tokenRing) release(token string, uncertain bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.slots {
		if r.slots[i].token == token {
			r.slots[i].spent, r.slots[i].uncertain = false, uncertain
		}
	}
}
