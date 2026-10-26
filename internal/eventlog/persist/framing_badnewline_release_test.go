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
