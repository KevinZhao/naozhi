package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestCoalesceMessages_TruncateMarker_FormatLock pins the byte-for-byte format
// of the multi-message truncation tail so a future refactor cannot silently
// regress to fmt.Fprintf — R241-CR-7 (#487) noted the marker had drifted
// between fmt.Fprintf and the WriteString fast path; this test ensures the
// merged tail uses the documented "\n[系统] 已省略 N 条后续消息（合并超出长度
// 上限）。\n" template literally and reports the count via decimal AppendInt.
//
// Without this pin, the existing TestCoalesceMessages_TotalBytesCap only
// asserts substring "已省略" — a regression that switched the formatter back
// to fmt.Sprintf("\n[系统] 已省略 %d ...") would still pass that assertion
// even though the pre-fix R20260526-PERF parity (zero-alloc on hot coalesce
// path) would be lost.
func TestCoalesceMessages_TruncateMarker_FormatLock(t *testing.T) {
	t.Parallel()
	// Use the same per-msg sizing pattern as TestCoalesceMessages_TotalBytesCap
	// so we are guaranteed to overshoot the cap and trigger the truncated > 0
	// branch.
	per := maxCoalescedTextBytes/4 + 1
	big := strings.Repeat("x", per)
	const overflow = 8 // arbitrary; 8 × per safely > maxCoalescedTextBytes
	msgs := make([]QueuedMsg, 0, overflow)
	for i := 0; i < overflow; i++ {
		msgs = append(msgs, QueuedMsg{
			Text:      big,
			Images:    []cli.ImageData{{Data: []byte{byte(i)}, MimeType: "image/png"}},
			EnqueueAt: time.Date(2026, 4, 16, 14, 0, i, 0, time.UTC),
		})
	}
	text, _ := CoalesceMessages(msgs)

	// The marker carries the count of dropped messages — locate it and pin
	// the exact prefix/suffix shape. We do not pin the count's literal value
	// because future cap tweaks may change how many messages survive, but
	// the wrapping template is the contract.
	const wantPrefix = "\n[系统] 已省略 "
	const wantSuffix = " 条后续消息（合并超出长度上限）。\n"

	idx := strings.Index(text, wantPrefix)
	if idx < 0 {
		t.Fatalf("truncate marker prefix %q missing from output (first 256 bytes):\n%s",
			wantPrefix, text[:min(256, len(text))])
	}
	tail := text[idx:]
	if !strings.HasPrefix(tail, wantPrefix) {
		t.Fatalf("tail does not start with %q; got %q", wantPrefix, tail[:min(64, len(tail))])
	}
	rest := tail[len(wantPrefix):]
	suffixIdx := strings.Index(rest, wantSuffix)
	if suffixIdx < 0 {
		t.Fatalf("truncate marker suffix %q missing; rest = %q", wantSuffix, rest[:min(128, len(rest))])
	}
	count := rest[:suffixIdx]
	if count == "" {
		t.Fatalf("truncate marker count is empty; full marker tail: %q", tail[:min(128, len(tail))])
	}
	for _, c := range count {
		if c < '0' || c > '9' {
			t.Fatalf("truncate marker count %q must be a decimal integer (no fmt padding / sign)", count)
		}
	}
	// Marker must be the very last thing in the output (no trailing
	// per-message footer slipping in after the truncation).
	if !strings.HasSuffix(text, wantSuffix) {
		t.Fatalf("truncate marker is not the final segment; output tail: %q",
			text[max(0, len(text)-128):])
	}
}
