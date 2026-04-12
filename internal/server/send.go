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
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
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
	// Notify ALL dashboard clients that this session is running so they can
	// auto-subscribe. Uses BroadcastSessionReady (sends to all authenticated
	// clients) instead of broadcastState (only subscribed clients), because
	// for new sessions nobody is subscribed yet.
	h.BroadcastSessionReady(key)
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

// sendParams holds parsed input for a session send request.
// Both HTTP and WebSocket callers construct this after their own input parsing.
type sendParams struct {
	Key       string
	Text      string
	Images    []cli.ImageData
	Workspace string
	ResumeID  string
}

// sessionSend validates and dispatches a send request.
// Returns (true, nil) if the request was a /clear or /new reset.
// Returns (false, err) if validation failed (workspace forbidden, etc.).
// Returns (false, nil) if accepted — a background goroutine handles the send.
//
// onAsyncError is called from the goroutine if GetOrCreate or guard timeout
// fails; it may be nil (HTTP path has no back-channel after 202).
func (h *Hub) sessionSend(p sendParams, onAsyncError func(string)) (bool, error) {
	key := p.Key

	// Handle /clear and /new — CLI built-in doesn't work in stream-json
	trimmed := strings.TrimSpace(p.Text)
	if trimmed == "/clear" || trimmed == "/new" {
		h.router.Reset(key)
		h.BroadcastSessionsUpdate()
		return true, nil
	}

	// Workspace validation
	var validatedWorkspace string
	if p.Workspace != "" {
		wsPath, err := validateWorkspace(p.Workspace, h.allowedRoot)
		if err != nil {
			return false, err
		}
		validatedWorkspace = wsPath
		if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
			h.router.SetWorkspace(key[:idx], wsPath)
		}
	}

	// Resume registration
	if p.ResumeID != "" && discovery.IsValidSessionID(p.ResumeID) {
		ws := validatedWorkspace
		if ws == "" {
			ws = h.router.DefaultWorkspace()
		}
		h.router.RegisterForResume(key, p.ResumeID, ws, "")
	}

	// Guard acquire/interrupt
	acquired := h.guard.TryAcquire(key)
	needInterrupt := !acquired
	if needInterrupt {
		h.router.InterruptSession(key)
		slog.Info("send: interrupted running session", "key", key)
	}

	// Background send
	text, images := p.Text, p.Images
	go func() {
		sendStart := time.Now()
		if needInterrupt {
			if !h.guard.AcquireTimeout(h.ctx, key, 2*time.Second) {
				slog.Error("send: interrupt timed out", "key", key)
				if onAsyncError != nil {
					onAsyncError("session busy, interrupt timed out")
				}
				return
			}
		}
		defer h.guard.Release(key)
		defer h.router.NotifyIdle()

		opts := buildSessionOpts(key, h.agents, h.projectMgr)
		sess, status, err := h.router.GetOrCreate(h.ctx, key, opts)
		if err != nil {
			slog.Error("send: get session", "key", key, "err", err)
			if onAsyncError != nil {
				onAsyncError(err.Error())
			}
			return
		}
		if status != session.SessionExisting {
			slog.Info("send: session spawned", "key", key, "status", status, "elapsed", time.Since(sendStart).Round(time.Millisecond))
		}

		if _, err := h.sendWithBroadcast(h.ctx, key, sess, text, images, nil); err != nil {
			slog.Error("send: send", "key", key, "err", err)
		} else if h.scheduler != nil && strings.HasPrefix(key, "cron:") {
			if err := h.scheduler.SetJobPrompt(strings.TrimPrefix(key, "cron:"), text); err != nil {
				slog.Warn("send: set cron prompt", "key", key, "err", err)
			}
		}
		slog.Info("send: turn complete", "key", key, "elapsed", time.Since(sendStart).Round(time.Millisecond))
	}()

	return false, nil
}
