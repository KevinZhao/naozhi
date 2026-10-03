package weixin

import (
	"hash/maphash"
	"sync"
)

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
	// send, of the message keyed msgKey, may have been delivered.
	uncertain bool
	msgKey    uint64
}

// lease is one send attempt's token. reserved is false for the last-resort
// reuse of a spent token. retry: the slot is uncertain for this very message.
// uncertain: the slot is uncertain for another message.
type lease struct {
	token     string
	reserved  bool
	retry     bool
	uncertain bool
}

var msgKeySeed = maphash.MakeSeed()

// messageKey identifies a message across ReplyWithRetry's attempts, which
// resend the same text; two messages with equal text share a key.
func messageKey(text string) uint64 { return maphash.String(msgKeySeed, text) }

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
// so a concurrent Reply picks another. Order: the token uncertain for msgKey,
// so a retry stays on the token its earlier send may have spent; the newest
// unspent token; the newest token uncertain for another message. With nothing
// unspent it returns the newest token unreserved as the last resort. ok is
// false for an empty ring.
func (r *tokenRing) take(msgKey uint64) (l lease, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	const otherUncertain, fresh, retry = 1, 2, 3
	pick, rank := -1, 0
	for i := len(r.slots) - 1; i >= 0; i-- {
		s := r.slots[i]
		if s.spent {
			continue
		}
		k := fresh
		switch {
		case s.uncertain && s.msgKey == msgKey:
			k = retry
		case s.uncertain:
			k = otherUncertain
		}
		if k > rank {
			pick, rank = i, k
		}
	}
	if pick >= 0 {
		s := &r.slots[pick]
		l = lease{token: s.token, reserved: true, retry: rank == retry, uncertain: rank == otherUncertain}
		s.spent = true
		return l, true
	}
	if n := len(r.slots); n > 0 {
		return lease{token: r.slots[n-1].token}, true
	}
	return lease{}, false
}

// release hands a reserved token back unspent. uncertain records that the
// send of the message keyed msgKey got no verdict and may have been delivered.
func (r *tokenRing) release(token string, uncertain bool, msgKey uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.slots {
		if s := &r.slots[i]; s.token == token {
			s.spent, s.uncertain, s.msgKey = false, uncertain, msgKey
		}
	}
}
