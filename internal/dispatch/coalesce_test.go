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
