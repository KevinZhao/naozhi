// send.go contains sendWithBroadcast, the canonical wrapper for sending
// messages to a session with dashboard state notifications.
//
// All entry points that send user messages (IM, HTTP API, WebSocket) should
// use this rather than calling sess.Send directly, so the dashboard receives
// running/ready state transitions. The only exception is cron (internal/cron),
// which runs in a separate package and uses sess.Send directly since cron
// jobs are background tasks with their own notification path (BroadcastCronResult).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// sendWithBroadcast wraps sess.Send with dashboard state broadcasts.
// Broadcasts "running" before send, and the final session snapshot after send.
// This is the canonical implementation; Server.sendWithBroadcast delegates here.
//
// sess must be non-nil; callers must check the error from GetOrCreate first.
func (h *Hub) sendWithBroadcast(
	ctx context.Context,
	key string,
	sess *session.ManagedSession,
	text string,
	images []cli.ImageData,
	onEvent cli.EventCallback,
) (*cli.SendResult, error) {
	// Notify dashboard that this session is now running so clients can
	// auto-subscribe. BroadcastSessionsUpdate is debounced (200ms) so
	// the session list refreshes once the turn completes.
	h.broadcastState(key, "running", "")
	h.BroadcastSessionsUpdate()

	result, err := sess.Send(ctx, text, images, onEvent)

	// Broadcast final state (ready or suspended) after Send completes.
	if rs := h.router.GetSession(key); rs != nil {
		snap := rs.Snapshot()
		h.broadcastState(key, snap.State, snap.DeathReason)
	}
	h.BroadcastSessionsUpdate()

	return result, err
}

// sendWithBroadcast is a nil-safe delegation to Hub.sendWithBroadcast.
// When the dashboard is not registered (hub is nil, e.g. in tests or headless mode),
// falls back to a direct sess.Send without broadcasts.
//
// sess must be non-nil; callers must check the error from GetOrCreate first.
func (s *Server) sendWithBroadcast(
	ctx context.Context,
	key string,
	sess *session.ManagedSession,
	text string,
	images []cli.ImageData,
	onEvent cli.EventCallback,
) (*cli.SendResult, error) {
	if sess == nil {
		return nil, fmt.Errorf("sendWithBroadcast: session is nil")
	}
	if s.hub != nil {
		return s.hub.sendWithBroadcast(ctx, key, sess, text, images, onEvent)
	}
	return sess.Send(ctx, text, images, onEvent)
}

// trySaveCronPrompt checks if key is a cron session (cron:{id}) and, if the
// corresponding cron job has no prompt yet, saves text as the prompt and
// unpauses the job. Called after the first successful dashboard send.
func (s *Server) trySaveCronPrompt(key, text string) {
	if s.scheduler == nil || !strings.HasPrefix(key, "cron:") {
		return
	}
	cronID := strings.TrimPrefix(key, "cron:")
	if err := s.scheduler.SetJobPrompt(cronID, text); err != nil {
		slog.Warn("set cron prompt", "key", key, "err", err)
	}
}
