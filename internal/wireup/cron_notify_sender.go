// cron_notify_sender.go adapts the live platform map into cron.NotifySender /
// cron.PlatformReplier so internal/cron never imports internal/platform. Thin
// delegation only: chunking / retry / telemetry stay in cron's notifyTarget.

package wireup

import (
	"context"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

// Compile-time guards so method-set drift surfaces here, next to the adapters.
var (
	_ cron.NotifySender    = platformNotifySender{}
	_ cron.PlatformReplier = platformReplier{}
)

// platformNotifySender implements cron.NotifySender over the live platform
// map, which is read-only after boot.
type platformNotifySender struct {
	platforms map[string]platform.Platform
}

// newPlatformNotifySender wraps the live platform map as a cron.NotifySender.
func newPlatformNotifySender(platforms map[string]platform.Platform) cron.NotifySender {
	return platformNotifySender{platforms: platforms}
}

// Lookup resolves a platform name to a PlatformReplier; ok=false when the
// platform is unregistered or nil.
func (s platformNotifySender) Lookup(name string) (cron.PlatformReplier, bool) {
	p := s.platforms[name]
	if p == nil {
		return nil, false
	}
	return platformReplier{p: p}, true
}

// platformReplier adapts a single platform.Platform to cron.PlatformReplier.
type platformReplier struct {
	p platform.Platform
}

// MaxReplyLength returns the platform's split length, falling back to
// platform.DefaultMaxReplyLen when the platform reports <=0.
func (r platformReplier) MaxReplyLength() int {
	if n := r.p.MaxReplyLength(); n > 0 {
		return n
	}
	return platform.DefaultMaxReplyLen
}

// Split delegates to platform.SplitText so chunk boundaries are unchanged.
func (r platformReplier) Split(text string, maxLen int) []string {
	return platform.SplitText(text, maxLen)
}

// Reply delegates to platform.ReplyWithRetry; ctx passes through unchanged so
// stopCtx cancellation still reaches a hung webhook (#799).
func (r platformReplier) Reply(ctx context.Context, chatID, text string) (string, error) {
	return platform.ReplyWithRetry(ctx, r.p, platform.OutgoingMessage{
		ChatID: chatID,
		Text:   text,
	}, limits.PlatformReplyMaxAttempts)
}

// UsesSingleUseReplyToken exposes the single-use-token bit (e.g. WeChat iLink)
// so cron collapses a long result into one truncated message — chunks 2..N
// would be lost once the first send consumes the token (#2181).
func (r platformReplier) UsesSingleUseReplyToken() bool {
	return platform.UsesSingleUseReplyToken(r.p)
}
