package persist

import (
	"bufio"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// makeBadNewlineFrame builds a frame whose declared length is satisfied
// (exactly n body bytes are present) but whose Nth+1 separator byte is
// NOT the expected '\n'. This drives ReadFramedBody into the
// "body[n] != '\n'" branch (the corrupted/truncated-frame recovery
// path) that returns ErrMalformedFrame.
func makeBadNewlineFrame(body string) string {
	// <len>\n<body><wrong-separator>
	return fmt.Sprintf("%d\n%sX", len(body), body)
}

// TestReadFramedBody_BadNewline_ReturnsMalformed confirms the
// bad-newline branch surfaces ErrMalformedFrame with a nil body, for a
// range of body sizes (table-driven). This is the contract the three
// readers (ReadRecord / spliceLog / naozhilog source) rely on to abort
// the scan at the first corrupt frame.
func TestReadFramedBody_BadNewline_ReturnsMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"tiny", "{}"},
		{"small", `{"a":"b"}`},
		{"medium", `{"v":1,"seq":7,"type":"entry","entry":{"k":"` + strings.Repeat("y", 40) + `"}}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			frame := makeBadNewlineFrame(tc.body)
			gotBody, gotN, err := ReadFramedBody(bufio.NewReader(strings.NewReader(frame)))
			if !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("err = %v, want ErrMalformedFrame", err)
			}
			if gotBody != nil {
				t.Errorf("body = %v, want nil on error", gotBody)
			}
			if gotN != 0 {
				t.Errorf("n = %d, want 0 on error", gotN)
			}
		})
	}
}

// TestReadFramedBody_BadNewline_ReleasesBuffer is the regression guard
// for #1950: the bad-newline branch must return its pool-borrowed buffer
// via ReleaseFramedBody before returning the error, exactly like the
// sibling io.ReadFull error branch. Before the fix the branch dropped
// the only reference to the buffer (the caller receives nil and cannot
// release it), so the pooled buffer was never returned.
//
// Check: drain the pool, then in a GC-disabled, thread-pinned window
// repeatedly { malformed read; Get } and assert that at least one Get
// observes a buffer sized for the frame (cap >= n+1). If the error path
// releases its buffer, such a buffer reaches the pool and is observed
// within a few iterations; if it never releases, no iteration can ever
// observe a cap >= n+1 buffer (the pool only ever New's the 4096 default).
//
// Why a retry loop rather than a single Get: sync.Pool is per-P. Even
// with GC off, a single Put-then-Get can miss if the goroutine is
// rescheduled onto a different P between the two (the new P's local pool
// is empty, so Get falls back to New). macOS CI runners with many cores
// hit this reliably (#1950 flake). LockOSThread reduces P migration and
// the loop tolerates the residual misses without ever producing a false
// positive for the no-release regression.
func TestReadFramedBody_BadNewline_ReleasesBuffer(t *testing.T) {
	// Use a body LARGER than the pool's default New capacity (4096) so a
	// released (grown) buffer is unmistakably distinct from a fresh pool
	// entry: only an actual Put on the error path can make a cap >= 8193
	// buffer available.
	body := strings.Repeat("z", 8192)
	wantCap := len(body) + 1

	// Pin the goroutine to one OS thread and keep GC off for the whole
	// window so a released buffer is not evicted (GC clears the pool) and
	// the per-P pool we Put into is the same one we Get from.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	frame := makeBadNewlineFrame(body)
	for iter := 0; iter < 256; iter++ {
		// Drain pooled buffers so the only large entry that can be present
		// after the read is the one ReadFramedBody released this iteration.
		for i := 0; i < 64; i++ {
			_ = framedBodyPool.Get()
		}

		_, _, err := ReadFramedBody(bufio.NewReader(strings.NewReader(frame)))
		if !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("err = %v, want ErrMalformedFrame", err)
		}

		bp := framedBodyPool.Get().(*[]byte)
		if cap(*bp) >= wantCap {
			return // observed the released buffer — fix is present.
		}
	}
	t.Fatalf("no Get observed a cap >= %d buffer across 256 iterations: "+
		"bad-newline branch never released its pool buffer (#1950 regression)",
		wantCap)
}
