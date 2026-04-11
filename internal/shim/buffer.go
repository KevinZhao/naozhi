package shim

import (
	"log/slog"
	"sync"
)

// RingBuffer stores stdout lines with global sequence numbers.
// Thread-safe. When full (by line count or byte size), the oldest lines are evicted.
type RingBuffer struct {
	mu       sync.Mutex
	lines    []bufLine
	head     int // next write position (circular)
	count    int // current number of stored lines
	maxLines int
	maxBytes int64
	curBytes int64
	seq      int64 // next sequence number to assign
}

type bufLine struct {
	seq  int64
	data []byte
}

// NewRingBuffer creates a ring buffer with the given limits.
func NewRingBuffer(maxLines int, maxBytes int64) *RingBuffer {
	if maxLines <= 0 {
		maxLines = 10000
	}
	if maxBytes <= 0 {
		maxBytes = 50 * 1024 * 1024 // 50MB
	}
	return &RingBuffer{
		lines:    make([]bufLine, maxLines),
		maxLines: maxLines,
		maxBytes: maxBytes,
	}
}

// Push appends a line to the buffer, evicting the oldest if necessary.
// Returns the assigned sequence number.
func (b *RingBuffer) Push(data []byte) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	assigned := b.seq

	// Evict oldest to stay within byte limit
	for b.count > 0 && b.curBytes+int64(len(data)) > b.maxBytes {
		b.evictOldest()
	}

	// Drop oversized lines that exceed maxBytes on their own
	if b.count == 0 && int64(len(data)) > b.maxBytes {
		slog.Warn("dropping oversized line from ring buffer", "size", len(data), "max", b.maxBytes)
		return assigned
	}

	// Evict oldest if at line capacity
	if b.count >= b.maxLines {
		b.evictOldest()
	}

	// Copy data to avoid holding references to external buffers
	copied := make([]byte, len(data))
	copy(copied, data)

	b.lines[b.head] = bufLine{seq: assigned, data: copied}
	b.head = (b.head + 1) % b.maxLines
	if b.count < b.maxLines {
		b.count++
	}
	b.curBytes += int64(len(copied))

	return assigned
}

func (b *RingBuffer) evictOldest() {
	if b.count == 0 {
		return
	}
	oldest := (b.head - b.count + b.maxLines) % b.maxLines
	b.curBytes -= int64(len(b.lines[oldest].data))
	b.lines[oldest] = bufLine{}
	b.count--
}

// LinesSince returns all lines with seq > afterSeq, in order.
func (b *RingBuffer) LinesSince(afterSeq int64) []bufLine {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.count == 0 {
		return nil
	}

	var result []bufLine
	start := (b.head - b.count + b.maxLines) % b.maxLines
	for i := 0; i < b.count; i++ {
		idx := (start + i) % b.maxLines
		if b.lines[idx].seq > afterSeq {
			result = append(result, b.lines[idx])
		}
	}
	return result
}

// SeqRange returns the oldest and newest sequence numbers in the buffer.
// Returns (0, 0) if empty.
func (b *RingBuffer) SeqRange() (oldest, newest int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.count == 0 {
		return 0, 0
	}

	oldestIdx := (b.head - b.count + b.maxLines) % b.maxLines
	newestIdx := (b.head - 1 + b.maxLines) % b.maxLines
	return b.lines[oldestIdx].seq, b.lines[newestIdx].seq
}

// Count returns the number of lines currently in the buffer.
func (b *RingBuffer) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// Bytes returns the total byte size of all lines in the buffer.
func (b *RingBuffer) Bytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.curBytes
}
