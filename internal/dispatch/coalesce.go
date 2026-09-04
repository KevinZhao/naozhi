package dispatch

import (
	"strconv"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/textutil"
)

// coalescePrefix is the header injected before burst-coalesced messages so
// Claude knows what follows was sent while it processed the previous one.
const coalescePrefix = "[以下是用户在你处理上一条消息期间追加发送的内容]\n"

// coalescePrefixLen is the compile-time byte length of coalescePrefix.
const coalescePrefixLen = len(coalescePrefix)

// maxCoalescedTextBytes is a *soft* cap on the merged prompt size: the loop
// checks `b.Len() >= cap` *before* appending, so output can exceed it by at
// most one per-message payload (1 MB) plus framing — worst case ~5 MB, under
// the shim's 12 MB maxStdinLineBytes ceiling. Without it a queue of MaxDepth=N
// could amplify N × 1 MB into a single CLI stdin write. Source of truth is
// internal/limits so upstream's reverse RPC need not import dispatch.
const maxCoalescedTextBytes = limits.MaxCoalescedText

// CoalesceMessages merges multiple queued messages into a single prompt.
//
// Single message: returned as-is. Multiple messages: prefixed with a system
// hint and timestamped. If the coalesced result would exceed
// maxCoalescedTextBytes, later messages are dropped with a visible truncation
// marker — their images are still preserved so attached screenshots are not
// silently lost. Images from all messages are concatenated in order.
func CoalesceMessages(msgs []QueuedMsg) (string, []cli.Attachment) {
	if len(msgs) == 0 {
		return "", nil
	}
	if len(msgs) == 1 {
		// Defense in depth: per-message cap is enforced at every ingress, but
		// guard here so an oversized message never reaches CLI stdin. Cut at
		// a rune boundary so json.Marshal never sees a half codepoint.
		t := msgs[0].Text
		if len(t) > maxCoalescedTextBytes {
			cut := textutil.TruncateAtRuneBoundary(t, maxCoalescedTextBytes)
			t = t[:cut] + "\n[系统] 内容已截断。\n"
		}
		return t, msgs[0].Images
	}

	var b strings.Builder
	// Pre-grow sized to the actual payload (capped at maxCoalescedTextBytes)
	// rather than the hard cap, so a small burst doesn't allocate 4 MB.
	const framingOverheadPerMsg = 64 // "[HH:MM] " + "\n" + markers
	estimate := coalescePrefixLen + 128
	for _, m := range msgs {
		estimate += len(m.Text) + framingOverheadPerMsg
	}
	if estimate > maxCoalescedTextBytes {
		estimate = maxCoalescedTextBytes
	}
	b.Grow(estimate)
	b.WriteString(coalescePrefix)

	var allImages []cli.Attachment

	truncated := 0
	for _, m := range msgs {
		// Collect images unconditionally so attached screenshots survive
		// even when the text is trimmed for prompt size.
		allImages = append(allImages, m.Images...)

		if b.Len() >= maxCoalescedTextBytes {
			truncated++
			continue
		}
		// Emit "[HH:MM] " by hand: avoids fmt reflection and time's layout
		// parser on this per-message hot loop.
		b.WriteByte('\n')
		b.WriteByte('[')
		hh := m.EnqueueAt.Hour()
		mm := m.EnqueueAt.Minute()
		b.WriteByte(byte('0' + hh/10))
		b.WriteByte(byte('0' + hh%10))
		b.WriteByte(':')
		b.WriteByte(byte('0' + mm/10))
		b.WriteByte(byte('0' + mm%10))
		b.WriteString("] ")
		b.WriteString(m.Text)
		b.WriteByte('\n')
	}
	if truncated > 0 {
		// Allocation-light tail: strconv.AppendInt writes straight into b.
		b.WriteString("\n[系统] 已省略 ")
		var tmp [20]byte
		b.Write(strconv.AppendInt(tmp[:0], int64(truncated), 10))
		b.WriteString(" 条后续消息（合并超出长度上限）。\n")
	}

	return b.String(), allImages
}
