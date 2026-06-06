// cron_notify_sender.go adapts the live platform map into the cron-local
// cron.NotifySender / cron.PlatformReplier interfaces so internal/cron never
// imports internal/platform.
//
// #725: cron.notifyTarget used to index a map[string]platform.Platform and
// call platform.SplitText / platform.ReplyWithRetry directly, which pinned the
// internal/cron → internal/platform reverse dependency. wireup is the seam
// that already knows both type universes (it forwards deps.Platforms into the
// scheduler), mirroring cron_router_adapter.go's cron↔session translation
// (R260528-ARCH-23 / #1382). The adapter is a thin delegation: no chunking /
// retry / telemetry logic lives here — those stay in cron's notifyTarget loop.

package wireup

import (
	"context"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
)

// Compile-time interface-satisfaction guards so a method-set drift surfaces
// here in the adapter file rather than at the newPlatformNotifySender return /
// Lookup call site — mirroring cron_router_adapter.go's `var _ cron.SessionRouter`
// precedent.
var (
	_ cron.NotifySender    = platformNotifySender{}
	_ cron.PlatformReplier = platformReplier{}
)

// platformNotifySender implements cron.NotifySender over the live platform
// map. The map is the same value handed to the scheduler; it is read-only
// after boot.
type platformNotifySender struct {
	platforms map[string]platform.Platform
}

// newPlatformNotifySender wraps the live platform map as a cron.NotifySender.
func newPlatformNotifySender(platforms map[string]platform.Platform) cron.NotifySender {
	return platformNotifySender{platforms: platforms}
}

// Lookup resolves a platform name to a PlatformReplier. ok=false when the
// platform is not registered (or its entry is nil), which keeps cron's
// "platform not found" WARN on the same path it fired before.
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

// MaxReplyLength returns the platform's per-message split length, falling back
// to platform.DefaultMaxReplyLen when the platform reports <=0 — the exact
// fallback cron.notifyTarget applied inline before #725.
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

// Reply delegates to platform.ReplyWithRetry, passing ctx through unchanged so
// the R243-SEC-14 (#799) stopCtx cancellation still propagates into a hung
// webhook, and using the shared limits.PlatformReplyMaxAttempts retry budget.
func (r platformReplier) Reply(ctx context.Context, chatID, text string) (string, error) {
	return platform.ReplyWithRetry(ctx, r.p, platform.OutgoingMessage{
		ChatID: chatID,
		Text:   text,
	}, limits.PlatformReplyMaxAttempts)
}
