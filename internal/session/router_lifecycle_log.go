package session

import (
	"context"
	"log/slog"
)

// sessionLifecycleLevel is the slog level used for the session lifecycle
// audit events (spawned / reset / removed / expired).
//
// R214-CODE-5 (#422): these four events were previously emitted with bare,
// independently-worded slog.Info calls scattered across router_lifecycle.go
// and router_cleanup.go. The open question was whether they should stay at
// Info (audit-first: an operator can reconstruct the full session timeline
// from the log) or drop to Debug (noise reduction in high-volume
// deployments).
//
// Decision: keep them at Info. Each event fires exactly once per session
// lifecycle transition — NOT per user message (the spawn happens once, the
// reset/removed/expired each terminate the session) — so the volume is
// bounded by session churn, not message throughput. The audit trail they
// provide is load-bearing for the "why did this chat lose its context"
// support flow.
//
// Centralising the level in one variable makes the decision a single tunable
// point: an operator who genuinely needs to silence the lifecycle stream can
// flip this to slog.LevelDebug in one place rather than editing four call
// sites, and every lifecycle line carries the same structured `event`
// attribute so it can be filtered/aggregated downstream.
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
