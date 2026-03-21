package platform

import "sync"

// Dedup tracks seen event IDs for idempotency.
type Dedup struct {
	mu   sync.Mutex
	seen map[string]struct{}
	cap  int
}

// NewDedup creates a dedup tracker with a max capacity.
func NewDedup(cap int) *Dedup {
	if cap <= 0 {
		cap = 10000
	}
	return &Dedup{
		seen: make(map[string]struct{}, cap),
		cap:  cap,
	}
}

// Seen returns true if the event ID was already seen, and records it.
func (d *Dedup) Seen(eventID string) bool {
	if eventID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[eventID]; ok {
		return true
	}

	// Simple eviction: clear when full
	if len(d.seen) >= d.cap {
		d.seen = make(map[string]struct{}, d.cap)
	}
	d.seen[eventID] = struct{}{}
	return false
}
