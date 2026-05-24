package dispatch

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/limits"
)

// coalescePrefix is the header injected before the burst-coalesced messages to
// inform Claude that what follows is a set of follow-up messages sent while it
// was processing the previous one.  Declared as a typed const so the compiler
// evaluates len(coalescePrefix) at compile time (coalescePrefixLen) rather
// than re-computing the byte count on every hot-path call.
const coalescePrefix = "[以下是用户在你处理上一条消息期间追加发送的内容]\n"

// coalescePrefixLen is the compile-time byte length of coalescePrefix.
// Go spec §Constant expressions: len of a constant string is a constant.
const coalescePrefixLen = len(coalescePrefix)

// maxCoalescedTextBytes is a *soft* cap on the merged prompt size. The
// coalesce loop checks `b.Len() >= cap` *before* appending the current
// message, so the final length can exceed the cap by at most one
// maxWSSendTextBytes per-message payload (1 MB) plus a small
// header/trailer constant. Worst-case output: ~5 MB, safely under the
// shim's 12 MB maxStdinLineBytes ceiling. Per-message ingress caps
// (maxWSSendTextBytes on WS/HTTP, the IM-side inbound cap on platform
// handlers) bound individual queued entries; without this cap a queue
// with MaxDepth=N could amplify N × 1 MB into a single CLI stdin write.
// R60-GO-M4.
//
// Source of truth lives in internal/limits so cross-trust-boundary
// callers (upstream connector's reverse RPC) don't have to reverse-import
// dispatch just to read the cap (R228-ARCH-9). Aliased here as a package
// alias so the dense coalesce hot loop reads naturally.
const maxCoalescedTextBytes = limits.MaxCoalescedText

// CoalesceMessages merges multiple queued messages into a single prompt.
//
// Single message: returned as-is (per-message cap already enforced at ingress).
// Multiple messages: prefixed with a system hint and timestamped, so Claude
// understands these are follow-up messages sent while it was processing. If
// the coalesced result would exceed maxCoalescedTextBytes, later messages
// are dropped with a visible truncation marker — their images are still
// preserved so attached screenshots are not silently lost.
//
// Images from all messages are concatenated in order.
func CoalesceMessages(msgs []QueuedMsg) (string, []cli.ImageData) {
	if len(msgs) == 0 {
		return "", nil
	}
	if len(msgs) == 1 {
		// Defense in depth: per-message cap is enforced at every ingress
		// path (WS handleSend, HTTP dashboard_send, IM platform adapters),
		// but if any new ingress ever skips the cap, this guard ensures a
		// single oversized message cannot reach CLI stdin. Truncate at the
		// byte boundary; a trailing partial UTF-8 rune is harmless to the
		// CLI prompt. R61-GO-5.
		t := msgs[0].Text
		if len(t) > maxCoalescedTextBytes {
			t = t[:maxCoalescedTextBytes] + "\n[系统] 内容已截断。\n"
		}
		return t, msgs[0].Images
	}

	var b strings.Builder
	// Pre-grow once, sized to the *actual* payload rather than the hard cap.
	// A 4 MB Grow on a 2-message burst of 100-byte texts would allocate 4 MB
	// just to write ~300 bytes. Summing actual payload sizes + ~64-byte
	// per-message framing overhead keeps peak alloc proportional to the
	// coalesce burst's real size while still capping at maxCoalescedTextBytes
	// to prevent the exponential-growth pattern (1M→2M→4M→8M with reallocs).
	// R-coalesce-adaptive-grow (was R68-PERF-M6).
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

	// Let allImages grow via append's exponential policy instead of
	// pre-counting. Most queued messages carry zero images, so the
	// pre-count scan was paying O(N) twice for a savings the single
	// append-realloc (log₂N growth) didn't justify. The common multi-msg
	// burst is ≤10 messages; append growth is 1→2→4→8→16 = 5 reallocs
	// worst case which is negligible on this infrequent path. R61-PERF-5.
	var allImages []cli.ImageData

	truncated := 0
	for _, m := range msgs {
		// Image collection happens unconditionally — the per-message image
		// count is already bounded at ingress (10/req) and retaining them
		// preserves the user's attached screenshots even if the textual
		// narrative has to be trimmed for prompt-size reasons.
		allImages = append(allImages, m.Images...)

		if b.Len() >= maxCoalescedTextBytes {
			truncated++
			continue
		}
		// R228-PERF-19: direct WriteString avoids fmt's reflection path on
		// the per-message hot loop. Format("15:04") still allocates a
		// small string but that's unavoidable with time.Time.
		b.WriteByte('\n')
		b.WriteByte('[')
		b.WriteString(m.EnqueueAt.Format("15:04"))
		b.WriteString("] ")
		b.WriteString(m.Text)
		b.WriteByte('\n')
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "\n[系统] 已省略 %d 条后续消息（合并超出长度上限）。\n", truncated)
	}

	return b.String(), allImages
}
