package shim

// Test-only accessors for unexported / internally-tracked state.
// Production code does not need these; they exist so *_test.go files
// in this package can assert internal counters without exposing the
// API surface to real callers.

// Bytes returns the total byte size of all lines currently in the buffer.
// Test-only: production never inspects this — eviction is driven by the
// internal counter inside evictOldest.
func (b *RingBuffer) Bytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.curBytes
}
