package dispatch

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

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
const maxCoalescedTextBytes = 4 * 1024 * 1024

// MaxCoalescedTextBytes exports the soft cap so cross-trust-boundary
// RPC handlers (e.g. upstream connector's `send` case) can reject
// oversized payloads before they reach CoalesceMessages. Returning the
// internal constant keeps the source of truth single.
func MaxCoalescedTextBytes() int { return maxCoalescedTextBytes }

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
	estimate := len("[以下是用户在你处理上一条消息期间追加发送的内容]\n") + 128
	for _, m := range msgs {
		estimate += len(m.Text) + framingOverheadPerMsg
	}
	if estimate > maxCoalescedTextBytes {
		estimate = maxCoalescedTextBytes
	}
	b.Grow(estimate)
	b.WriteString("[以下是用户在你处理上一条消息期间追加发送的内容]\n")

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
		// Direct Fprintf into the builder — avoids the intermediate string
		// that fmt.Sprintf would allocate on every queued message.
		fmt.Fprintf(&b, "\n[%s] %s\n", m.EnqueueAt.Format("15:04"), m.Text)
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "\n[系统] 已省略 %d 条后续消息（合并超出长度上限）。\n", truncated)
	}

	return b.String(), allImages
}
