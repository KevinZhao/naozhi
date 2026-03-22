package platform

import "sync"

// Dedup tracks seen event IDs for idempotency.
// Uses a dual-bucket strategy to avoid GC spikes from bulk eviction.
type Dedup struct {
	mu       sync.Mutex
	current  map[string]struct{}
	previous map[string]struct{}
	cap      int
}

// NewDedup creates a dedup tracker with a max capacity per bucket.
func NewDedup(cap int) *Dedup {
	if cap <= 0 {
		cap = 10000
	}
	return &Dedup{
		current:  make(map[string]struct{}, cap),
		previous: make(map[string]struct{}, cap),
		cap:      cap,
	}
}

// Seen returns true if the event ID was already seen, and records it.
func (d *Dedup) Seen(eventID string) bool {
	if eventID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.current[eventID]; ok {
		return true
	}
	if _, ok := d.previous[eventID]; ok {
		d.current[eventID] = struct{}{}
		return true
	}

	// Rotate buckets when current is full
	if len(d.current) >= d.cap {
		d.previous = d.current
		d.current = make(map[string]struct{}, d.cap)
	}
	d.current[eventID] = struct{}{}
	return false
}
