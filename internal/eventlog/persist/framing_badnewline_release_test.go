package persist

import (
	"bufio"
	"errors"
	"fmt"
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
// Deterministic check: drain the pool, perform the malformed read in a
// tight window, then Get from the pool. A Put on the error path means a
// buffer sized for the frame (cap >= n+1) is available immediately;
// without the fix the pool yields only freshly-New'd zero-cap buffers.
func TestReadFramedBody_BadNewline_ReleasesBuffer(t *testing.T) {
	// Use a body LARGER than the pool's default New capacity (4096) so a
	// released (grown) buffer is unmistakably distinct from a fresh pool
	// entry: only an actual Put on the error path can make a cap >= 8193
	// buffer available immediately.
	body := strings.Repeat("z", 8192)
	wantCap := len(body) + 1

	// Drain any pre-existing pooled buffers so the only entry that can be
	// present after the read is the one ReadFramedBody released.
	for i := 0; i < 64; i++ {
		_ = framedBodyPool.Get()
	}

	frame := makeBadNewlineFrame(body)
	_, _, err := ReadFramedBody(bufio.NewReader(strings.NewReader(frame)))
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("err = %v, want ErrMalformedFrame", err)
	}

	// Immediately reclaim the released buffer. sync.Pool may evict under
	// GC pressure, but in this tight single-goroutine window a Put is
	// overwhelmingly returned by the very next Get.
	bp := framedBodyPool.Get().(*[]byte)
	if cap(*bp) < wantCap {
		t.Fatalf("pool returned cap=%d < %d after bad-newline read: "+
			"buffer was not released back to framedBodyPool (#1950 regression)",
			cap(*bp), wantCap)
	}
}
