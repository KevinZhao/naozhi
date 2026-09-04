// Package ratelimit provides a thread-safe per-key token-bucket limiter with a
// bounded LRU of entries and lazy TTL expiry, shared by the per-IP limiters in
// internal/server and internal/node.
//
// Entries live in a recency-ordered doubly-linked list (head = most recent,
// tail = eviction candidate) plus a map for O(1) lookup. TTL is lazy: an entry
// whose lastSeen is older than TTL is reset on next access; no periodic scan.
// The zero value is not usable; call New.
package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config configures a Limiter; zero values pick defaults.
type Config struct {
	// Rate is the token-bucket refill rate. Required.
	Rate rate.Limit
	// Burst is the token-bucket burst size. Required (>=1).
	Burst int
	// MaxKeys caps distinct keys held; the LRU key is evicted beyond it. Default 1000.
	MaxKeys int
	// TTL is the idle duration after which an entry is reset. Default 10 minutes.
	TTL time.Duration
}

const (
	defaultMaxKeys = 1000
	defaultTTL     = 10 * time.Minute
)

// Limiter is a per-key token-bucket limiter backed by a bounded LRU; use New.
type Limiter struct {
	cfg Config

	// nowFn is injected in tests (nil → time.Now) so lazy-TTL tests step time
	// instead of sleeping past a short TTL.
	nowFn func() time.Time

	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front = most recently used, back = least
}

func (l *Limiter) now() time.Time {
	if l.nowFn != nil {
		return l.nowFn()
	}
	return time.Now()
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

// Allow reports whether the bucket for key has a token, refreshing last-seen
// and promoting it to the LRU head. Empty keys are rejected so callers that
// failed to resolve a client IP don't share one "" bucket. O(1) on all paths.
func (l *Limiter) Allow(key string) bool {
	if key == "" {
		return false
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.entries[key]; ok {
		ent := el.Value.(*entry)
		if now.Sub(ent.lastSeen) > l.cfg.TTL {
			// Lazy expiry: long-idle keys don't carry stale debt into a new burst.
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

// Len returns the number of tracked keys (tests / observability).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lru.Len()
}
