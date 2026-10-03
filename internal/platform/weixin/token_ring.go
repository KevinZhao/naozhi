package weixin

import "sync"

// tokenRingCap bounds the context_tokens kept per user. Two covers the
// collect-mode case (answer + merged follow-up); the slack absorbs a burst.
const tokenRingCap = 4

// tokenRing holds a user's most recent inbound context_tokens. iLink treats
// each token as single-use, so overwriting one cached value let the first
// reply of a turn spend the token a later reply needed (#3003).
type tokenRing struct {
	mu        sync.Mutex
	slots     []tokenSlot // oldest first, len <= tokenRingCap
	updatedNs int64       // stamp of the newest push; drives TTL eviction
	evicted   bool        // set by evictIfIdle; a pusher must start a new ring
}

type tokenSlot struct {
	token string
	spent bool
}

// push records an inbound token as the newest slot, dropping the oldest past
// tokenRingCap. A redelivered token keeps its slot and spent state. Returns
// false when the ring was evicted concurrently.
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

// take returns the token for the next send attempt: the newest unspent one,
// reserved (marked spent) so a concurrent Reply picks another, or the newest
// token even if spent as the last resort (reserved=false). ok is false for an
// empty ring.
func (r *tokenRing) take() (token string, reserved, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.slots) - 1; i >= 0; i-- {
		if !r.slots[i].spent {
			r.slots[i].spent = true
			return r.slots[i].token, true, true
		}
	}
	if n := len(r.slots); n > 0 {
		return r.slots[n-1].token, false, true
	}
	return "", false, false
}

// release hands a reserved token back when the send never reached a verdict.
func (r *tokenRing) release(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.slots {
		if r.slots[i].token == token {
			r.slots[i].spent = false
		}
	}
}
