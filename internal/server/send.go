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
	"runtime/debug"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/dispatch"
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

	// Broadcast final state after Send completes.
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

// sendAckStatus describes the immediate ack status for a queued send.
//   - "accepted": caller became the owner; message is processing now.
//   - "queued":   session was busy; message is queued behind the active turn.
type sendAckStatus string

const (
	sendAckAccepted sendAckStatus = "accepted"
	sendAckQueued   sendAckStatus = "queued"
	// sendAckBusy is returned when the session is busy but the queue is
	// disabled (MaxDepth<=0) so the message cannot even be buffered. The
	// client should retry rather than assume the message will arrive.
	sendAckBusy sendAckStatus = "busy"
)

// sessionSend validates and dispatches a send request.
// Returns (true, "", nil) if the request was a /clear or /new reset.
// Returns (false, "", err) if validation failed (workspace forbidden, etc.).
// Returns (false, "accepted", nil) when we owned the send turn.
// Returns (false, "queued",   nil) when the session was busy and the message
// was enqueued behind the active turn — a background drain loop will process it
// after the current turn completes, coalescing with any other queued messages.
//
// onAsyncError is called from the owner goroutine if GetOrCreate fails; it may
// be nil (HTTP path has no back-channel after ack).
// maxSessionKeyLen caps the client-supplied session key so a long or
// malformed key cannot bloat router state, log lines, or session-update
// broadcasts. Realistic keys (platform:chatType:chatID:agentID) stay well
// under 256 bytes.
const maxSessionKeyLen = 512

func (h *Hub) sessionSend(p sendParams, onAsyncError func(string)) (bool, sendAckStatus, error) {
	key := p.Key
	if len(key) == 0 || len(key) > maxSessionKeyLen {
		return false, "", fmt.Errorf("invalid key length")
	}
	// Reject control characters and newlines — they propagate into log lines
	// and session IDs without adding value.
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < 0x20 || c == 0x7f {
			return false, "", fmt.Errorf("invalid key character")
		}
	}

	// Handle /clear and /new — CLI built-in doesn't work in stream-json.
	// Also clear any pending queue so stale follow-ups don't hit the fresh session.
	// Case-insensitive so CJK mobile IMEs that auto-capitalize the first letter
	// ("/Clear" / "/New") still reset. Mirrors dispatch.normalizeSlashCommand's
	// leading-token lowercasing used on the IM path.
	trimmed := strings.ToLower(strings.TrimSpace(p.Text))
	if trimmed == "/clear" || trimmed == "/new" {
		if h.queue != nil {
			h.queue.Discard(key)
		}
		h.router.Reset(key)
		h.BroadcastSessionsUpdate()
		return true, "", nil
	}

	// Workspace validation
	var validatedWorkspace string
	if p.Workspace != "" {
		wsPath, err := validateWorkspace(p.Workspace, h.allowedRoot)
		if err != nil {
			return false, "", err
		}
		validatedWorkspace = wsPath
		// Require a non-empty chat-key prefix before the final ':'. A key of the
		// form ":agentID" (idx==0) would otherwise persist the empty string as
		// a workspace override, overriding the default for every subsequent
		// GetWorkspace("") lookup.
		if idx := strings.LastIndexByte(key, ':'); idx > 0 {
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

	// Fallback to legacy guard path when no queue is configured (tests, headless).
	if h.queue == nil {
		return h.sessionSendLegacy(p, onAsyncError)
	}

	qm := dispatch.QueuedMsg{
		Text:      p.Text,
		Images:    p.Images,
		EnqueueAt: time.Now(),
	}
	isOwner, enqueued, gen := h.queue.Enqueue(key, qm)
	if !isOwner {
		if !enqueued {
			// Queue disabled (MaxDepth<=0) and session is busy — the
			// message is dropped. Surface this so the client knows to retry
			// instead of waiting for a drain that never owns this message.
			slog.Debug("send: message dropped (session busy, queue disabled)", "key", key)
			return false, sendAckBusy, nil
		}
		// Busy — message was accepted into the queue; owner's ownerLoop will
		// pick it up on its next drain tick.
		slog.Debug("send: message queued (session busy)", "key", key)
		return false, sendAckQueued, nil
	}

	// I'm the owner — spawn the drain loop. Gate with TrackSend so a send
	// arriving concurrent with Shutdown is declined cleanly rather than
	// escaping past sendWG.Wait.
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		// Drop ownership so a later Enqueue (post-restart) can re-own.
		// Discard bumps gen and clears the owner flag without re-invoking
		// ownerLoop. The caller will see sendAckBusy-equivalent behaviour.
		h.queue.Discard(key)
		return false, sendAckBusy, nil
	}
	go func() {
		defer release()
		h.ownerLoop(key, gen, qm, onAsyncError)
	}()
	return false, sendAckAccepted, nil
}

// ownerLoop processes the first send turn and then drains any messages that
// arrived while the turn was running, coalescing them into a single follow-up
// turn. Mirrors dispatch.Dispatcher.ownerLoop but integrates with the hub's
// broadcast + session routing.
//
// gen is the queue generation at enqueue time. If Discard (e.g., /new) bumps
// it mid-flight, DoneOrDrain returns nil and this loop exits cleanly.
// Caller must arrange sendWG accounting via TrackSend — ownerLoop does not
// touch sendWG directly so it can be launched with a defer-release closure.
func (h *Hub) ownerLoop(key string, gen uint64, first dispatch.QueuedMsg, onAsyncError func(string)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("ownerLoop panic", "key", key, "panic", r, "stack", string(debug.Stack()))
			if h.queue != nil {
				h.queue.Discard(key)
			}
		}
	}()
	defer h.router.NotifyIdle()

	h.runTurn(key, first.Text, first.Images, onAsyncError)

	// Drain loop: after each turn, wait collectDelay then drain.
	collectTimer := time.NewTimer(h.queue.CollectDelay())
	defer collectTimer.Stop()
	for {
		select {
		case <-h.ctx.Done():
			// Discard clears msgs and resets busy=false + bumps gen so a
			// fresh owner can be spawned by the next Enqueue after restart;
			// without this, the key would remain "busy" forever and queued
			// messages would never be processed.
			h.queue.Discard(key)
			return
		case <-collectTimer.C:
		}

		queued := h.queue.DoneOrDrain(key, gen)
		if queued == nil {
			return // empty or generation mismatch — stop.
		}

		text, images := dispatch.CoalesceMessages(queued)
		slog.Debug("send: processing queued messages", "key", key, "count", len(queued), "merged_len", len(text))
		// onAsyncError only applies to the first turn (one ack per request);
		// subsequent coalesced turns log failures without a back-channel.
		h.runTurn(key, text, images, nil)
		collectTimer.Reset(h.queue.CollectDelay())
	}
}

// runTurn executes one send turn: GetOrCreate + sendWithBroadcast.
func (h *Hub) runTurn(key, text string, images []cli.ImageData, onAsyncError func(string)) {
	sendStart := time.Now()
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
		// Spawn is an infrequent event (once per session lifecycle), so keep
		// it at Info for operator visibility. Other per-turn events are Debug.
		slog.Info("send: session spawned", "key", key, "status", status, "elapsed_ms", time.Since(sendStart).Milliseconds())
	}

	if _, err := h.sendWithBroadcast(h.ctx, key, sess, text, images, nil); err != nil {
		slog.Error("send: send", "key", key, "err", err)
	} else if h.scheduler != nil && strings.HasPrefix(key, "cron:") {
		if err := h.scheduler.SetJobPrompt(strings.TrimPrefix(key, "cron:"), text); err != nil {
			slog.Warn("send: set cron prompt", "key", key, "err", err)
		}
	}
	slog.Debug("send: turn complete", "key", key, "elapsed_ms", time.Since(sendStart).Milliseconds())
}

// sessionSendLegacy keeps the pre-queue guard/interrupt behavior for code paths
// that don't wire a MessageQueue (primarily tests). Production always configures
// a queue via Server.New.
func (h *Hub) sessionSendLegacy(p sendParams, onAsyncError func(string)) (bool, sendAckStatus, error) {
	key := p.Key

	acquired := h.guard.TryAcquire(key)
	needInterrupt := !acquired
	if needInterrupt {
		h.router.InterruptSession(key)
		slog.Debug("send: interrupted running session", "key", key)
	}

	text, images := p.Text, p.Images
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		if !needInterrupt {
			// We successfully acquired the guard above but will not spawn
			// the drain goroutine — release so a later enqueue (post-restart)
			// can re-acquire. needInterrupt=true means we never acquired,
			// only sent an interrupt which the CLI will observe regardless.
			h.guard.Release(key)
		}
		return false, sendAckBusy, nil
	}
	go func() {
		defer release()
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
		h.runTurn(key, text, images, onAsyncError)
	}()

	return false, sendAckAccepted, nil
}
