package cron

import "context"

// NotifySender resolves a platform name to the PlatformReplier that knows how
// to chunk and deliver an IM completion notice for that platform. It is the
// cron-local seam that keeps internal/cron free of any internal/platform
// import; the wireup layer owns the translation (#725). Lookup returns
// ok=false when the platform is not registered.
type NotifySender interface {
	Lookup(platform string) (PlatformReplier, bool)
}

// PlatformReplier is the per-platform send surface notifyTarget composes:
// it asks for the split length, splits the text, and delivers each chunk.
//
// Reply MUST pass ctx through unchanged to the underlying send so the
// stopCtx parent chain still short-circuits a hung webhook the moment
// Scheduler.Stop cancels it (#799).
type PlatformReplier interface {
	MaxReplyLength() int
	Split(text string, maxLen int) []string
	Reply(ctx context.Context, chatID, text string) (string, error)

	// UsesSingleUseReplyToken reports whether the platform can deliver only ONE
	// reply per inbound turn (single-use context token, e.g. WeChat iLink).
	// When true, notifyTarget MUST collapse a long result into one
	// rune-safe-truncated message instead of N chunks — otherwise only chunk 1
	// arrives (#2181).
	UsesSingleUseReplyToken() bool
}
