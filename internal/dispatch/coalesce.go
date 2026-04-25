package dispatch

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// maxCoalescedTextBytes caps the merged prompt size produced by
// CoalesceMessages. Per-message ingress caps (maxWSSendTextBytes=64KB on the
// WS path, the IM-side inbound cap on platform handlers) bound individual
// queued entries, but a queue with MaxDepth=N can still amplify N × 64KB
// into a single CLI stdin write. The shim hard-limits at 12MB
// (maxStdinLineBytes) but that is a much larger budget than legitimate
// operator use requires. 256KB comfortably fits any realistic follow-up
// burst (a reviewer pasting 3-4 coalesced stack traces) while keeping CLI
// stdin reasonably bounded. R60-GO-M4.
const maxCoalescedTextBytes = 256 * 1024

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
