package dispatch

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// CoalesceMessages merges multiple queued messages into a single prompt.
//
// Single message: returned as-is.
// Multiple messages: prefixed with a system hint and timestamped, so Claude
// understands these are follow-up messages sent while it was processing.
//
// Images from all messages are concatenated in order.
func CoalesceMessages(msgs []QueuedMsg) (string, []cli.ImageData) {
	if len(msgs) == 0 {
		return "", nil
	}
	if len(msgs) == 1 {
		return msgs[0].Text, msgs[0].Images
	}

	var b strings.Builder
	b.WriteString("[以下是用户在你处理上一条消息期间追加发送的内容]\n")

	// Pre-size allImages so append doesn't repeatedly grow/copy the slice when
	// several queued messages carry images. Most messages have zero images so
	// the upfront count scan is cheap.
	totalImages := 0
	for _, m := range msgs {
		totalImages += len(m.Images)
	}
	allImages := make([]cli.ImageData, 0, totalImages)
	for _, m := range msgs {
		// Direct Fprintf into the builder — avoids the intermediate string
		// that fmt.Sprintf would allocate on every queued message.
		fmt.Fprintf(&b, "\n[%s] %s\n", m.EnqueueAt.Format("15:04"), m.Text)
		allImages = append(allImages, m.Images...)
	}

	return b.String(), allImages
}
