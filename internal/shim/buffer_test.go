package shim

import (
	"fmt"
	"sync"
	"testing"
)

func TestRingBuffer_DefaultLimits(t *testing.T) {
	// NewRingBuffer with zero/negative values applies defaults
	b := NewRingBuffer(0, 0)
	if b.maxLines != 10000 {
		t.Errorf("maxLines = %d, want 10000", b.maxLines)
	}
	if b.maxBytes != 50*1024*1024 {
		t.Errorf("maxBytes = %d, want 50MB", b.maxBytes)
	}
}

func TestRingBuffer_SequenceMonotonicallyIncreases(t *testing.T) {
	b := NewRingBuffer(10, 1024)
	var seqs []int64
	for i := 0; i < 10; i++ {
		seqs = append(seqs, b.Push([]byte(fmt.Sprintf("line%d", i))))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("seq[%d]=%d not greater than seq[%d]=%d", i, seqs[i], i-1, seqs[i-1])
		}
	}
}

func TestRingBuffer_SequenceContinuesAfterEviction(t *testing.T) {
	b := NewRingBuffer(3, 10000)
	for i := 0; i < 5; i++ {
		b.Push([]byte("x"))
	}
	// Seq continues monotonically even with evictions
	seq6 := b.Push([]byte("y"))
	if seq6 != 6 {
		t.Errorf("seq after evictions = %d, want 6", seq6)
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	// Fill exactly to capacity, then push one more to cause wraparound
	const cap = 4
	b := NewRingBuffer(cap, 10000)

	for i := 0; i < cap; i++ {
		b.Push([]byte(fmt.Sprintf("line%d", i)))
	}
	if b.Count() != cap {
		t.Fatalf("Count() = %d, want %d before wrap", b.Count(), cap)
	}

	// Push one more: should evict seq=1 (line0)
	b.Push([]byte("new"))
	if b.Count() != cap {
		t.Fatalf("Count() = %d, want %d after wrap", b.Count(), cap)
	}

	oldest, newest := b.SeqRange()
	if oldest != 2 {
		t.Errorf("oldest seq = %d, want 2 after wrap", oldest)
	}
	if newest != int64(cap)+1 {
		t.Errorf("newest seq = %d, want %d after wrap", newest, cap+1)
	}

	// LinesSince(0) returns all 4 buffered
	lines := b.LinesSince(0)
	if len(lines) != cap {
		t.Errorf("LinesSince(0) = %d, want %d", len(lines), cap)
	}
}

func TestRingBuffer_LinesSince_MidRange(t *testing.T) {
	b := NewRingBuffer(10, 10000)
	for i := 1; i <= 8; i++ {
		b.Push([]byte(fmt.Sprintf("L%d", i)))
	}
	// Ask for lines after seq=5: should get seq 6,7,8
	lines := b.LinesSince(5)
	if len(lines) != 3 {
		t.Fatalf("LinesSince(5) = %d lines, want 3", len(lines))
	}
	if lines[0].seq != 6 || lines[1].seq != 7 || lines[2].seq != 8 {
		t.Errorf("unexpected seqs: %v", []int64{lines[0].seq, lines[1].seq, lines[2].seq})
	}
}

func TestRingBuffer_LinesSince_FutureSeq(t *testing.T) {
	b := NewRingBuffer(5, 1024)
	b.Push([]byte("a"))
	b.Push([]byte("b"))

	// Asking for lines after the newest seq returns nothing
	lines := b.LinesSince(100)
	if len(lines) != 0 {
		t.Errorf("LinesSince(100) = %d, want 0", len(lines))
	}
}

func TestRingBuffer_LinesSince_AllEvicted(t *testing.T) {
	// Push 10 lines into cap=3 buffer; lines 1-7 are evicted
	b := NewRingBuffer(3, 10000)
	for i := 0; i < 10; i++ {
		b.Push([]byte("x"))
	}
	// LinesSince(0) returns the 3 remaining lines (seq 8,9,10)
	lines := b.LinesSince(0)
	if len(lines) != 3 {
		t.Errorf("LinesSince(0) = %d, want 3", len(lines))
	}
	if lines[0].seq != 8 {
		t.Errorf("oldest remaining seq = %d, want 8", lines[0].seq)
	}
}

func TestRingBuffer_ByteLimit_EvictsMultiple(t *testing.T) {
	// Each line is 10 bytes, maxBytes=25: at most 2 fit
	b := NewRingBuffer(100, 25)

	b.Push([]byte("1234567890")) // seq=1, 10 bytes
	b.Push([]byte("1234567890")) // seq=2, total 20 bytes

	// Third push: 20+10=30 > 25, must evict seq=1 first, then seq=2 as well, then fit
	b.Push([]byte("1234567890")) // seq=3
	// After evicting 1 (20+10=30>25 evict seq1 -> 10+10=20<=25 OK)
	if b.Count() != 2 {
		t.Errorf("Count() = %d, want 2", b.Count())
	}
	if b.Bytes() != 20 {
		t.Errorf("Bytes() = %d, want 20", b.Bytes())
	}
	oldest, newest := b.SeqRange()
	if oldest != 2 || newest != 3 {
		t.Errorf("SeqRange = (%d, %d), want (2, 3)", oldest, newest)
	}
}

func TestRingBuffer_OversizedLineDrop(t *testing.T) {
	// A single line larger than maxBytes must be dropped (not stored)
	b := NewRingBuffer(100, 5)
	seq := b.Push([]byte("123456789012345")) // 15 bytes > maxBytes=5

	// Seq is still assigned (monotonic counter)
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	// But line must NOT be stored
	if b.Count() != 0 {
		t.Errorf("Count() = %d, want 0 (oversized line dropped)", b.Count())
	}
	if b.Bytes() != 0 {
		t.Errorf("Bytes() = %d, want 0", b.Bytes())
	}
}

func TestRingBuffer_OversizedLineAfterContent(t *testing.T) {
	// Oversized line arrives after existing content: evicts all, then drops
	b := NewRingBuffer(100, 10)
	b.Push([]byte("abcde"))                   // seq=1, 5 bytes
	seq := b.Push([]byte("ABCDEFGHIJKLMNOP")) // 16 bytes > maxBytes=10

	_ = seq
	// After evicting seq=1 to make room, 16>10 so it's dropped
	if b.Count() != 0 {
		t.Errorf("Count() = %d, want 0 after oversized drop", b.Count())
	}
}

// TestRingBuffer_OversizeDropCreatesSeqHole locks R55-CORR-003's protocol
// contract: a dropped oversize line still consumes its seq slot, so the
// subsequent successful push gets the next number — there is NO rewind
// that would make the replacement look like the dropped line never
// happened. Clients that reconnect and replay via LinesSince(afterSeq)
// see a gap where the oversize line used to be; the replay protocol
// delivers whatever lines ARE present whose seq > afterSeq, so the gap
// is invisible to well-behaved consumers. A future refactor that decides
// "don't bump seq on drop" to close the hole would silently re-use the
// seq for the next line, which is worse: two distinct stdout reads
// collapse into a single replay entry.
func TestRingBuffer_OversizeDropCreatesSeqHole(t *testing.T) {
	b := NewRingBuffer(100, 5)

	dropped := b.Push([]byte("123456789")) // 9 bytes > maxBytes=5, dropped
	if dropped != 1 {
		t.Fatalf("first push seq = %d, want 1", dropped)
	}
	if b.Count() != 0 {
		t.Fatalf("Count after drop = %d, want 0", b.Count())
	}

	// Subsequent push must advance past the dropped seq, not reuse it.
	// Use a small-enough line that fits under maxBytes=5.
	kept := b.Push([]byte("ok"))
	if kept != 2 {
		t.Errorf("next push after drop seq = %d, want 2 (hole preserved)", kept)
	}
	if b.Count() != 1 {
		t.Errorf("Count after drop+keep = %d, want 1", b.Count())
	}

	// LinesSince(0) returns only the kept line — the dropped seq's slot
	// yields no entry, so the replay payload is naturally sparse.
	lines := b.LinesSince(0)
	if len(lines) != 1 || lines[0].seq != 2 || string(lines[0].data) != "ok" {
		t.Errorf("LinesSince(0) = %+v, want [{seq:2 data:\"ok\"}]", lines)
	}
}

func TestRingBuffer_PushDataCopied(t *testing.T) {
	// Push must copy data, so mutating original after push has no effect
	b := NewRingBuffer(5, 1024)
	data := []byte("original")
	b.Push(data)

	data[0] = 'X' // mutate original

	lines := b.LinesSince(0)
	if len(lines) != 1 || string(lines[0].data) != "original" {
		t.Errorf("pushed data was not copied: got %q", lines[0].data)
	}
}

func TestRingBuffer_ConcurrentPushLinesSince(t *testing.T) {
	b := NewRingBuffer(100, 10*1024*1024)
	const goroutines = 8
	const pushesPerGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < pushesPerGoroutine; i++ {
				b.Push([]byte(fmt.Sprintf("goroutine%d-line%d", id, i)))
			}
		}(g)
	}

	// Concurrent readers
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = b.LinesSince(0)
				_ = b.Count()
				_, _ = b.SeqRange()
				_ = b.Bytes()
			}
		}()
	}

	wg.Wait()
	// All goroutines * pushes pushed but buffer is capped at 100
	if b.Count() > 100 {
		t.Errorf("Count() = %d exceeds maxLines=100", b.Count())
	}
}

func TestRingBuffer_SeqRange_SingleEntry(t *testing.T) {
	b := NewRingBuffer(5, 1024)
	b.Push([]byte("only"))
	oldest, newest := b.SeqRange()
	if oldest != 1 || newest != 1 {
		t.Errorf("SeqRange = (%d, %d), want (1, 1)", oldest, newest)
	}
}

func TestRingBuffer_ExactCapacity(t *testing.T) {
	// Fill to exactly maxLines with no eviction
	b := NewRingBuffer(3, 10000)
	b.Push([]byte("a"))
	b.Push([]byte("b"))
	b.Push([]byte("c"))

	if b.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", b.Count())
	}
	oldest, newest := b.SeqRange()
	if oldest != 1 || newest != 3 {
		t.Errorf("SeqRange = (%d, %d), want (1, 3)", oldest, newest)
	}
}

func TestRingBuffer_Bytes_AfterEviction(t *testing.T) {
	b := NewRingBuffer(3, 10000)
	b.Push([]byte("ab"))   // 2 bytes
	b.Push([]byte("cde"))  // 3 bytes, total 5
	b.Push([]byte("fghi")) // 4 bytes, total 9

	// Fourth push evicts oldest (2 bytes)
	b.Push([]byte("jk")) // 2 bytes, total after evict: 9-2+2=9
	expected := int64(3 + 4 + 2)
	if b.Bytes() != expected {
		t.Errorf("Bytes() = %d, want %d after eviction", b.Bytes(), expected)
	}
}
