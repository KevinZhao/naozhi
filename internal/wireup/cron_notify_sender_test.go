package wireup

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

// recordingPlatform is a minimal platform.Platform that records the ctx and
// message handed to Reply and counts invocations, so the adapter test can
// assert platformReplier.Reply delegates to platform.ReplyWithRetry with the
// caller's ctx + the shared retry budget passed through unchanged (#725).
type recordingPlatform struct {
	maxLen int

	mu       sync.Mutex
	gotCtx   context.Context
	gotMsg   platform.OutgoingMessage
	calls    int
	failWith error // when set, Reply returns this on every attempt
}

func (p *recordingPlatform) Name() string                                           { return "feishu" }
func (p *recordingPlatform) RegisterRoutes(*http.ServeMux, platform.MessageHandler) {}
func (p *recordingPlatform) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.gotCtx = ctx
	p.gotMsg = msg
	if p.failWith != nil {
		return "", p.failWith
	}
	return "msg-id", nil
}
func (p *recordingPlatform) EditMessage(context.Context, string, string) error { return nil }
func (p *recordingPlatform) MaxReplyLength() int                               { return p.maxLen }

// TestPlatformNotifySender_LookupHitDelegatesReply pins that Lookup of a
// registered platform returns ok=true and the returned PlatformReplier.Reply
// passes the caller ctx + OutgoingMessage through to platform.ReplyWithRetry.
func TestPlatformNotifySender_LookupHitDelegatesReply(t *testing.T) {
	t.Parallel()
	p := &recordingPlatform{maxLen: 4000}
	sender := newPlatformNotifySender(map[string]platform.Platform{"feishu": p})

	r, ok := sender.Lookup("feishu")
	if !ok {
		t.Fatal("Lookup(feishu) ok=false, want true for a registered platform")
	}

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	id, err := r.Reply(ctx, "chat-x", "hello")
	if err != nil {
		t.Fatalf("Reply returned err=%v, want nil", err)
	}
	if id != "msg-id" {
		t.Errorf("Reply msgID = %q, want msg-id", id)
	}
	if p.calls != 1 {
		t.Errorf("underlying platform.Reply called %d times, want 1", p.calls)
	}
	// ctx passthrough: the exact caller ctx (carrying the sentinel) must reach
	// the platform, so the R243-SEC-14 (#799) stopCtx cancellation propagates.
	if got := p.gotCtx.Value(ctxKey{}); got != "sentinel" {
		t.Errorf("ctx not passed through to platform.Reply; sentinel = %v, want \"sentinel\"", got)
	}
	if p.gotMsg.ChatID != "chat-x" || p.gotMsg.Text != "hello" {
		t.Errorf("OutgoingMessage = %+v, want ChatID=chat-x Text=hello", p.gotMsg)
	}
}

// TestPlatformNotifySender_ReplyUsesSharedRetryBudget pins that Reply routes
// through platform.ReplyWithRetry with limits.PlatformReplyMaxAttempts — a
// transient failure is retried up to the shared budget rather than once.
func TestPlatformNotifySender_ReplyUsesSharedRetryBudget(t *testing.T) {
	t.Parallel()
	p := &recordingPlatform{maxLen: 4000, failWith: errors.New("transient")}
	sender := newPlatformNotifySender(map[string]platform.Platform{"feishu": p})
	r, ok := sender.Lookup("feishu")
	if !ok {
		t.Fatal("Lookup(feishu) ok=false")
	}
	if _, err := r.Reply(context.Background(), "chat-x", "hi"); err == nil {
		t.Fatal("Reply err=nil, want the transient error after retries exhausted")
	}
	if p.calls != limits.PlatformReplyMaxAttempts {
		t.Errorf("platform.Reply attempts = %d, want limits.PlatformReplyMaxAttempts (%d) — Reply must route through ReplyWithRetry with the shared budget",
			p.calls, limits.PlatformReplyMaxAttempts)
	}
}

// TestPlatformNotifySender_LookupMiss pins ok=false for unregistered and nil
// platform entries — the path that keeps cron's "platform not found" WARN.
func TestPlatformNotifySender_LookupMiss(t *testing.T) {
	t.Parallel()
	sender := newPlatformNotifySender(map[string]platform.Platform{
		"feishu":   &recordingPlatform{maxLen: 4000},
		"nilentry": nil,
	})
	if _, ok := sender.Lookup("missing"); ok {
		t.Error("Lookup(missing) ok=true, want false for an unregistered platform")
	}
	if _, ok := sender.Lookup("nilentry"); ok {
		t.Error("Lookup(nilentry) ok=true, want false for a nil platform entry")
	}
}

// TestPlatformNotifySender_SplitMatchesPlatform pins that Split delegates to
// platform.SplitText so chunk boundaries are identical to the pre-#725 inline
// call.
func TestPlatformNotifySender_SplitMatchesPlatform(t *testing.T) {
	t.Parallel()
	p := &recordingPlatform{maxLen: 8}
	sender := newPlatformNotifySender(map[string]platform.Platform{"feishu": p})
	r, _ := sender.Lookup("feishu")

	const text = "hello world this is a longer message that splits"
	got := r.Split(text, 8)
	want := platform.SplitText(text, 8)
	if len(got) != len(want) {
		t.Fatalf("Split produced %d chunks, want %d (must match platform.SplitText)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Split chunk[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPlatformNotifySender_MaxReplyLengthFallback pins the DefaultMaxReplyLen
// fallback the adapter owns (#725): a platform reporting <=0 yields the
// package default, mirroring the inline guard cron.notifyTarget used before.
func TestPlatformNotifySender_MaxReplyLengthFallback(t *testing.T) {
	t.Parallel()
	zero := &recordingPlatform{maxLen: 0}
	sender := newPlatformNotifySender(map[string]platform.Platform{"feishu": zero})
	r, _ := sender.Lookup("feishu")
	if got := r.MaxReplyLength(); got != platform.DefaultMaxReplyLen {
		t.Errorf("MaxReplyLength() = %d for a <=0 platform, want DefaultMaxReplyLen (%d)", got, platform.DefaultMaxReplyLen)
	}

	custom := &recordingPlatform{maxLen: 1234}
	sender2 := newPlatformNotifySender(map[string]platform.Platform{"feishu": custom})
	r2, _ := sender2.Lookup("feishu")
	if got := r2.MaxReplyLength(); got != 1234 {
		t.Errorf("MaxReplyLength() = %d, want 1234 (positive platform value must pass through)", got)
	}
}
