package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// permanentSendErr is a platform.PermanentError so ReplyWithRetry aborts
// immediately (no backoff delays) — keeping the failure-accounting test fast.
type permanentSendErr struct{}

func (permanentSendErr) Error() string     { return "send rejected (permanent)" }
func (permanentSendErr) IsPermanent() bool { return true }

// TestSendOutboundImages_FailureCountsInHealthMetrics is the R202606j-CR-007
// regression guard: a failed outbound image send must bump the per-dispatcher
// sendFailCount (surfaced via /health), matching the text reply paths
// (single-use token / split chunk / error reply). Before the fix the image
// loop only logged a warning, so an image-only reply lost after retries was
// invisible to operator dashboards.
func TestSendOutboundImages_FailureCountsInHealthMetrics(t *testing.T) {
	fp := &fakePlatform{replyErr: permanentSendErr{}}
	d := &Dispatcher{}

	beforeLocal := d.sendFailCount.Load()

	imgs := []platform.Image{
		{Data: []byte("a"), MimeType: "image/png"},
		{Data: []byte("b"), MimeType: "image/png"},
	}
	d.sendOutboundImages(context.Background(), fp, ReplyDest{ChatID: "chat-1"}, imgs)

	if got := d.sendFailCount.Load() - beforeLocal; got != int64(len(imgs)) {
		t.Errorf("sendFailCount delta = %d, want %d (one per failed image)", got, len(imgs))
	}
}

// TestSendOutboundImages_SuccessNoFailCount pins the negative case: when every
// image send succeeds, neither failure counter moves.
func TestSendOutboundImages_SuccessNoFailCount(t *testing.T) {
	fp := &fakePlatform{} // no replyErr → Reply succeeds
	d := &Dispatcher{}

	beforeLocal := d.sendFailCount.Load()

	d.sendOutboundImages(context.Background(), fp, ReplyDest{ChatID: "chat-1"}, []platform.Image{
		{Data: []byte("a"), MimeType: "image/png"},
	})

	if got := d.sendFailCount.Load() - beforeLocal; got != 0 {
		t.Errorf("sendFailCount moved by %d on success, want 0", got)
	}
	if fp.replyCount() != 1 {
		t.Errorf("expected 1 image reply delivered, got %d", fp.replyCount())
	}
}
