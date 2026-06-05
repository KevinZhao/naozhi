package cron

import "context"

// NotifySender resolves a platform name to the PlatformReplier that knows how
// to chunk and deliver an IM completion notice for that platform.
//
// #725: introducing this cron-local interface severs the last
// internal/cron → internal/platform production import edge. notifyTarget
// previously reached into a map[string]platform.Platform and called
// platform.SplitText / platform.ReplyWithRetry directly, which pinned the
// reverse dependency. The wireup layer now owns the platform translation
// (internal/wireup/cron_notify_sender.go), mirroring the cronSessionAdapter
// precedent (R20260527122801-ARCH-1 / #1318) where cron speaks only
// cron-local types and wireup casts to the concrete platform.* universe.
//
// Lookup returns ok=false when the platform is not registered; notifyTarget
// keeps its existing "platform not found" WARN on that path. The chunk-cap /
// partial-telemetry / stopCtx short-circuit / empty-text guards all stay in
// cron's notifyTarget loop — the interface deliberately exposes the same
// MaxReplyLength / Split / Reply primitives notifyTarget composed before, so
// no behaviour moves into the adapter.
type NotifySender interface {
	Lookup(platform string) (PlatformReplier, bool)
}

// PlatformReplier is the per-platform send surface notifyTarget composes:
// it asks for the split length, splits the text, and delivers each chunk.
// The concrete implementation (wireup's platformReplier) delegates to
// platform.MaxReplyLength / platform.SplitText / platform.ReplyWithRetry so
// the chunk loop's behaviour is identical to the pre-#725 inline calls.
//
// Reply MUST pass ctx through unchanged to the underlying send so the
// R243-SEC-14 (#799) stopCtx parent chain still short-circuits a hung
// webhook the moment Scheduler.Stop cancels it.
type PlatformReplier interface {
	MaxReplyLength() int
	Split(text string, maxLen int) []string
	Reply(ctx context.Context, chatID, text string) (string, error)
}
