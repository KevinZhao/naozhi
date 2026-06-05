package cron

import (
	"context"

	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

// notify_sender_fake_test.go provides a cron-test-local NotifySender /
// PlatformReplier that wraps a platform.Platform so the existing notifyTarget
// regression tests can keep their well-exercised fake platforms after #725
// moved notifyTarget off the platform map onto the cron.NotifySender
// interface.
//
// The wrapper delegates exactly as wireup's production platformReplier does
// (platform.SplitText / platform.ReplyWithRetry with
// limits.PlatformReplyMaxAttempts and the DefaultMaxReplyLen fallback), so the
// chunk-loop / partial-telemetry / stopCtx / empty-text behaviour under test
// is identical to production. The platform import here is test-only and does
// NOT re-introduce the production cron→platform edge that
// no_platform_import_test.go pins (`go list -deps` excludes _test.go imports).

// fakeNotifySender maps platform names to platform.Platform fakes and exposes
// them through cron.NotifySender.
type fakeNotifySender struct {
	platforms map[string]platform.Platform
}

func newFakeNotifySender(platforms map[string]platform.Platform) NotifySender {
	return fakeNotifySender{platforms: platforms}
}

func (f fakeNotifySender) Lookup(name string) (PlatformReplier, bool) {
	p := f.platforms[name]
	if p == nil {
		return nil, false
	}
	return fakePlatformReplier{p: p}, true
}

// storeFakeNotifySender is a tiny helper that publishes a fakeNotifySender
// over the given platform map into s's atomic config snapshot, mirroring the
// pre-#725 `s.configMapsPtr.Store(&cronConfigMaps{platforms: ...})` idiom.
func storeFakeNotifySender(s *Scheduler, platforms map[string]platform.Platform) {
	s.configMapsPtr.Store(&cronConfigMaps{
		notifySender: newFakeNotifySender(platforms),
	})
}

type fakePlatformReplier struct {
	p platform.Platform
}

func (r fakePlatformReplier) MaxReplyLength() int {
	if n := r.p.MaxReplyLength(); n > 0 {
		return n
	}
	return platform.DefaultMaxReplyLen
}

func (r fakePlatformReplier) Split(text string, maxLen int) []string {
	return platform.SplitText(text, maxLen)
}

func (r fakePlatformReplier) Reply(ctx context.Context, chatID, text string) (string, error) {
	return platform.ReplyWithRetry(ctx, r.p, platform.OutgoingMessage{
		ChatID: chatID,
		Text:   text,
	}, limits.PlatformReplyMaxAttempts)
}
