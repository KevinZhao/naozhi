package weixin

import (
	"hash/maphash"
	"sync"
	"time"
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
	// uncertain ties the slot to the message keyed msgKey as of uncertainNs.
	// Unspent: that message's send got no verdict and may have landed.
	// Spent: another message was rejected on it, so that send most likely
	// landed. Either way it binds msgKey only for retryAffinityWindow.
	uncertain   bool
	msgKey      uint64
	uncertainNs int64
}

// retryAffinityWindow is how long an uncertain slot stays bound to its
// message. It must outlast the gap between ReplyWithRetry attempts (backoff
// capped near 5s); past it, an equal text is a different message.
const retryAffinityWindow = 30 * time.Second

// lease is one send attempt's token. reserved is false for the last-resort
// reuse of a spent token. retry: the slot was uncertain for this message.
// landed: no send is needed, this message most likely already landed.
// uncertain: the slot was uncertain for another message, whose key and stamp
// are otherKey and otherNs.
type lease struct {
	token     string
	reserved  bool
	retry     bool
	landed    bool
	uncertain bool
	otherKey  uint64
	otherNs   int64
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
// so a concurrent Reply picks another. Order: a slot showing msgKey landed;
// the token uncertain for msgKey, so a retry stays on the token its earlier
// send may have spent; the newest unspent token; the newest token uncertain
// for another message. With nothing unspent it returns the newest token
// unreserved as the last resort. ok is false for an empty ring.
func (r *tokenRing) take(msgKey uint64, nowNs int64) (l lease, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	const otherUncertain, fresh, retry, landed = 1, 2, 3, 4
	pick, rank := -1, 0
	for i := len(r.slots) - 1; i >= 0; i-- {
		s := r.slots[i]
		bound := s.uncertain && s.msgKey == msgKey &&
			nowNs-s.uncertainNs <= int64(retryAffinityWindow)
		k := fresh
		switch {
		case s.spent && bound:
			k = landed
		case s.spent:
			continue
		case bound:
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
		l = lease{token: s.token, reserved: rank != landed, retry: rank == retry, landed: rank == landed,
			uncertain: rank == otherUncertain, otherKey: s.msgKey, otherNs: s.uncertainNs}
		s.spent, s.uncertain = true, false
		return l, true
	}
	if n := len(r.slots); n > 0 {
		return lease{token: r.slots[n-1].token}, true
	}
	return lease{}, false
}

// release hands a reserved token back unspent. uncertain records that the
// send of the message keyed msgKey got no verdict at nowNs.
func (r *tokenRing) release(token string, uncertain bool, msgKey uint64, nowNs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.slots {
		if s := &r.slots[i]; s.token == token {
			s.spent, s.uncertain, s.msgKey, s.uncertainNs = false, uncertain, msgKey, nowNs
		}
	}
}

// markLanded records on a spent token that the message keyed msgKey most
// likely landed on it, so its retry stops instead of going to a fresh token.
func (r *tokenRing) markLanded(token string, msgKey uint64, sinceNs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.slots {
		if s := &r.slots[i]; s.token == token && s.spent {
			s.uncertain, s.msgKey, s.uncertainNs = true, msgKey, sinceNs
		}
	}
}
