package shim

import (
	"log/slog"
	"sort"
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

// defaultRingMaxLines is the fallback line cap when maxLines<=0; 10k covers a
// typical Claude turn with replay margin. ManagerConfig.BufferSize references
// it directly (single source of truth).
const defaultRingMaxLines = 10000

// defaultRingMaxBytes is the fallback byte cap (50 MiB); whichever cap trips
// first drives eviction. Shared with ManagerConfig.MaxBufBytes.
const defaultRingMaxBytes int64 = 50 * 1024 * 1024

// NewRingBuffer creates a ring buffer with the given limits. Non-positive
// values fall back to defaultRingMaxLines / defaultRingMaxBytes.
func NewRingBuffer(maxLines int, maxBytes int64) *RingBuffer {
	if maxLines <= 0 {
		maxLines = defaultRingMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = defaultRingMaxBytes
	}
	return &RingBuffer{
		lines:    make([]bufLine, maxLines),
		maxLines: maxLines,
		maxBytes: maxBytes,
	}
}

// Push appends a line to the buffer, evicting the oldest if necessary.
// Returns the assigned sequence number.
//
// Protocol contract: seq advances on EVERY call, including the oversize-drop
// branch, so clients see a monotonic seq space with holes. The replay
// protocol tolerates holes (LinesSince returns only present seqs; callers
// treat the delta as "delivered slice", not contiguous); a skipped seq is
// more honest than pretending the read never happened.
func (b *RingBuffer) Push(data []byte) int64 {
	// Copy before taking the lock (ownership: the caller's slice is the
	// bufio.Scanner line, reused on the next Scan).
	copied := append([]byte(nil), data...)

	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	assigned := b.seq

	// Check oversize BEFORE evicting: otherwise a single line > maxBytes wipes
	// the whole ring and is dropped anyway (#2182). seq was still bumped, so
	// the drop leaves a hole per the monotonic-seq contract.
	if int64(len(copied)) > b.maxBytes {
		slog.Warn("dropping oversized line from ring buffer", "size", len(copied), "max", b.maxBytes)
		return assigned
	}

	// Evict oldest to stay within byte limit
	for b.count > 0 && b.curBytes+int64(len(copied)) > b.maxBytes {
		b.evictOldest()
	}

	// Evict oldest if at line capacity
	if b.count >= b.maxLines {
		b.evictOldest()
	}

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
	// Caught-up caller (afterSeq >= b.seq): short-circuit the O(n) scan so an
	// idle reconnect doesn't block every concurrent Push under the mutex.
	if afterSeq >= b.seq {
		return nil
	}

	// Entries along the logical window [0, count) are in strictly increasing
	// seq (Push assigns monotonically and appends at head, even after wrap),
	// so binary-search the first match and copy the tail in one pre-sized
	// append: O(log n) + one copy under the mutex instead of O(n).
	start := (b.head - b.count + b.maxLines) % b.maxLines
	first := sort.Search(b.count, func(i int) bool {
		idx := (start + i) % b.maxLines
		return b.lines[idx].seq > afterSeq
	})
	matches := b.count - first
	if matches <= 0 {
		return nil
	}
	result := make([]bufLine, 0, matches)
	for i := first; i < b.count; i++ {
		idx := (start + i) % b.maxLines
		result = append(result, b.lines[idx])
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
