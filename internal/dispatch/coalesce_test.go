package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestCoalesceMessages_Empty(t *testing.T) {
	text, images := CoalesceMessages(nil)
	if text != "" || images != nil {
		t.Fatalf("expected empty, got %q, %v", text, images)
	}
}

func TestCoalesceMessages_Single(t *testing.T) {
	imgs := []cli.ImageData{{Data: []byte("img"), MimeType: "image/png"}}
	text, images := CoalesceMessages([]QueuedMsg{
		{Text: "hello", Images: imgs, EnqueueAt: time.Date(2026, 4, 16, 14, 2, 0, 0, time.UTC)},
	})
	if text != "hello" {
		t.Fatalf("single message should be returned as-is, got %q", text)
	}
	if len(images) != 1 {
		t.Fatalf("images len = %d, want 1", len(images))
	}
}

func TestCoalesceMessages_Multiple(t *testing.T) {
	msgs := []QueuedMsg{
		{Text: "帮我写个函数", EnqueueAt: time.Date(2026, 4, 16, 14, 2, 0, 0, time.UTC)},
		{Text: "要用Go", Images: []cli.ImageData{{Data: []byte("a"), MimeType: "image/png"}}, EnqueueAt: time.Date(2026, 4, 16, 14, 2, 30, 0, time.UTC)},
		{Text: "还有记得加测试", EnqueueAt: time.Date(2026, 4, 16, 14, 3, 0, 0, time.UTC)},
	}
	text, images := CoalesceMessages(msgs)

	if !strings.HasPrefix(text, "[以下是用户在你处理上一条消息期间追加发送的内容]") {
		t.Fatalf("missing prefix, got:\n%s", text)
	}
	if !strings.Contains(text, "[14:02] 帮我写个函数") {
		t.Fatalf("missing first message, got:\n%s", text)
	}
	if !strings.Contains(text, "[14:02] 要用Go") {
		t.Fatalf("missing second message, got:\n%s", text)
	}
	if !strings.Contains(text, "[14:03] 还有记得加测试") {
		t.Fatalf("missing third message, got:\n%s", text)
	}
	if len(images) != 1 {
		t.Fatalf("images len = %d, want 1", len(images))
	}
}

func TestCoalesceMessages_ImagesConcat(t *testing.T) {
	msgs := []QueuedMsg{
		{Text: "A", Images: []cli.ImageData{{Data: []byte("1")}}, EnqueueAt: time.Now()},
		{Text: "B", Images: []cli.ImageData{{Data: []byte("2")}, {Data: []byte("3")}}, EnqueueAt: time.Now()},
	}
	_, images := CoalesceMessages(msgs)
	if len(images) != 3 {
		t.Fatalf("images len = %d, want 3", len(images))
	}
}

// TestCoalesceMessages_TotalBytesCap verifies that the merged prompt stays
// bounded under maxCoalescedTextBytes even when many queued messages arrive.
// Pre-fix, N × 64KB queued messages produced N × 64KB merged prompts (up to
// ~256KB with MaxDepth=4, larger if MaxDepth grew). Now we cap the running
// size, drop the tail, and emit a truncation marker while preserving all
// images. R60-GO-M4.
func TestCoalesceMessages_TotalBytesCap(t *testing.T) {
	big := strings.Repeat("x", 64*1024)
	msgs := make([]QueuedMsg, 0, 8)
	for i := 0; i < 8; i++ {
		msgs = append(msgs, QueuedMsg{
			Text:      big,
			Images:    []cli.ImageData{{Data: []byte("i"), MimeType: "image/png"}},
			EnqueueAt: time.Date(2026, 4, 16, 14, 0, i, 0, time.UTC),
		})
	}
	text, images := CoalesceMessages(msgs)

	// Each image always flows through regardless of truncation so attached
	// screenshots are never silently lost.
	if len(images) != 8 {
		t.Errorf("images len = %d, want 8 (images must survive truncation)", len(images))
	}
	// The merged prompt must not blow past the intended cap plus the short
	// preamble + truncation marker. Keep a generous margin (2KB) for the
	// header line and the final trailer; never more than ~260KB total.
	if len(text) > maxCoalescedTextBytes+2*1024 {
		t.Errorf("merged text len = %d, exceeds cap %d + margin", len(text), maxCoalescedTextBytes)
	}
	// The user must be able to see that content was truncated rather than
	// silently missing messages.
	if !strings.Contains(text, "已省略") {
		t.Errorf("truncation marker missing from output (first 200 chars): %s", text[:200])
	}
}

// TestCoalesceMessages_SingleMessageTruncatesOversize covers R61-GO-5:
// a single oversize message must not bypass the coalesce cap even though
// ingress paths have their own gates. Defense in depth.
func TestCoalesceMessages_SingleMessageTruncatesOversize(t *testing.T) {
	big := strings.Repeat("y", maxCoalescedTextBytes+1024)
	msgs := []QueuedMsg{{
		Text:      big,
		EnqueueAt: time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
	}}
	text, _ := CoalesceMessages(msgs)
	if len(text) > maxCoalescedTextBytes+len("\n[系统] 内容已截断。\n")+4 {
		t.Errorf("single-message path did not truncate: len=%d, cap=%d", len(text), maxCoalescedTextBytes)
	}
	if !strings.Contains(text, "已截断") {
		t.Error("single-message truncation marker missing")
	}
}
