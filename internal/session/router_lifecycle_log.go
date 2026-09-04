package session

import (
	"context"
	"log/slog"
)

// sessionLifecycleLevel is the slog level used for the session lifecycle
// audit events (spawned / reset / removed / expired) (#422).
//
// Info, not Debug: each event fires once per lifecycle transition — NOT per
// user message — so volume is bounded by session churn, and the audit trail is
// load-bearing for the "why did this chat lose its context" support flow.
// Flip to slog.LevelDebug here to silence the whole stream.
const sessionLifecycleLevel = slog.LevelInfo

// logSessionLifecycle emits one structured session-lifecycle audit line. The
// `event` value is one of "spawned" / "reset" / "removed" / "expired"; extra
// supplies any event-specific key/value attributes (e.g. "active", "idle").
func logSessionLifecycle(event, key string, extra ...any) {
	args := make([]any, 0, 4+len(extra))
	args = append(args, "event", event, "key", key)
	args = append(args, extra...)
	slog.Log(context.Background(), sessionLifecycleLevel, "session "+event, args...)
}
