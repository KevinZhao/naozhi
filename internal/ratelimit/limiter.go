// Package ratelimit provides a thread-safe per-key token-bucket limiter
// with a bounded-size LRU cache of entries and lazy TTL expiry.
//
// It replaces two near-identical ad-hoc implementations in
// internal/server (per-IP HTTP limiter) and internal/node (per-IP
// /ws-node limiter). Consolidating the logic keeps eviction and
// expiry behaviour consistent across endpoints and gives both call
// sites O(1) eviction instead of the previous O(N) full-map scans.
//
// Design:
//
//   - Entries are kept in a doubly-linked list ordered by recency.
//     The most-recently-used entry is at the head; the tail is the
//     eviction candidate when MaxKeys is reached.
//
//   - A map[string]*list.Element provides O(1) lookup.
//
//   - TTL is applied lazily: when Allow hits an existing entry whose
//     lastSeen is older than TTL, the entry is treated as absent and
//     a fresh limiter is installed. No periodic scan runs.
//
// The zero value is not usable; call New.
package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config configures a Limiter. Zero values pick sensible defaults so
// callers only need to specify what they care about.
type Config struct {
	// Rate is the token-bucket refill rate. Required.
	Rate rate.Limit
	// Burst is the token-bucket burst size. Required (>=1).
	Burst int
	// MaxKeys caps the number of distinct keys held in memory.
	// When exceeded, the least-recently-used key is evicted.
	// Defaults to 1000 if zero.
	MaxKeys int
	// TTL is the idle duration after which an entry is considered stale
	// and its limiter is reset on next access. Defaults to 10 minutes
	// if zero.
	TTL time.Duration
}

const (
	defaultMaxKeys = 1000
	defaultTTL     = 10 * time.Minute
)

// Limiter is a thread-safe per-key token-bucket rate limiter backed by a
// bounded LRU with lazy expiry. Construct with New.
type Limiter struct {
	cfg Config

	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front = most recently used, back = least
}

type entry struct {
	key      string
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New returns a ready-to-use Limiter.
func New(cfg Config) *Limiter {
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = defaultMaxKeys
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	return &Limiter{
		cfg:     cfg,
		entries: make(map[string]*list.Element, cfg.MaxKeys),
		lru:     list.New(),
	}
}

// Allow reports whether the token bucket for key has a token available.
// It updates the key's last-seen timestamp and promotes it to the LRU
// head. Empty keys are rejected as a safety net so callers that failed
// to resolve a client IP don't share a single "" bucket.
//
// Complexity is O(1) on both hit and miss paths; eviction is also O(1).
func (l *Limiter) Allow(key string) bool {
	if key == "" {
		return false
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.entries[key]; ok {
		ent := el.Value.(*entry)
		if now.Sub(ent.lastSeen) > l.cfg.TTL {
			// Lazy expiry: reset the bucket so long-idle keys don't
			// carry stale debt into a new burst.
			ent.limiter = rate.NewLimiter(l.cfg.Rate, l.cfg.Burst)
		}
		ent.lastSeen = now
		l.lru.MoveToFront(el)
		return ent.limiter.Allow()
	}

	// Miss: evict LRU tail if at capacity, then insert fresh entry.
	if l.lru.Len() >= l.cfg.MaxKeys {
		if back := l.lru.Back(); back != nil {
			old := back.Value.(*entry)
			delete(l.entries, old.key)
			l.lru.Remove(back)
		}
	}
	ent := &entry{
		key:      key,
		limiter:  rate.NewLimiter(l.cfg.Rate, l.cfg.Burst),
		lastSeen: now,
	}
	l.entries[key] = l.lru.PushFront(ent)
	return ent.limiter.Allow()
}

// Len returns the current number of tracked keys. Intended for tests
// and observability; not a hot-path API.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lru.Len()
}
