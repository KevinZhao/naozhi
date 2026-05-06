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
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/osutil"
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
	return h.sendWithBroadcastPriority(ctx, key, sess, text, images, onEvent, "")
}

// sendWithBroadcastPriority is the passthrough-aware variant of
// sendWithBroadcast. When priority is non-empty (or when the dispatch layer
// asks for passthrough via context key), the session routes through
// SendPassthrough so multiple calls for the same session can run concurrently;
// otherwise the legacy serialized Send path is used. The broadcast calls are
// identical — they only signal "session active / state changed" to dashboard
// subscribers, which is agnostic to the concurrency model underneath.
//
// When the ctx carries dispatch.WithUrgent, the explicit `priority` argument
// is upgraded to "now" if not already set — this lets handlers opt into the
// urgent path without threading the priority field through every wrapper.
func (h *Hub) sendWithBroadcastPriority(
	ctx context.Context,
	key string,
	sess *session.ManagedSession,
	text string,
	images []cli.ImageData,
	onEvent cli.EventCallback,
	priority string,
) (*cli.SendResult, error) {
	h.BroadcastSessionReady(key)
	h.BroadcastSessionsUpdate()

	if priority == "" && dispatch.IsUrgent(ctx) {
		priority = "now"
	}

	var (
		result *cli.SendResult
		err    error
	)
	switch {
	case usePassthrough(ctx, sess):
		result, err = sess.SendPassthrough(ctx, text, images, onEvent, priority)
	case priority == "now":
		// ACP / legacy protocols: emulate urgent by interrupting the in-flight
		// turn before sending the new message. Best-effort; failures are not
		// fatal — the message still lands on the next turn.
		sess.InterruptViaControl()
		result, err = sess.Send(ctx, text, images, onEvent)
	default:
		result, err = sess.Send(ctx, text, images, onEvent)
	}

	if rs := h.router.GetSession(key); rs != nil {
		snap := rs.Snapshot()
		h.broadcastState(key, snap.State, snap.DeathReason)
	}
	h.BroadcastSessionsUpdate()

	return result, err
}

// usePassthrough reports whether this turn should take the passthrough path.
// Gate: ctx carries dispatch.WithPassthrough AND session reports support.
func usePassthrough(ctx context.Context, sess *session.ManagedSession) bool {
	if sess == nil || !sess.SupportsPassthrough() {
		return false
	}
	return dispatch.IsPassthrough(ctx)
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
	// Headless mode (no hub): still route through passthrough when caller
	// set the ctx marker and session supports it.
	if usePassthrough(ctx, sess) {
		return sess.SendPassthrough(ctx, text, images, onEvent, "")
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
	Backend   string // optional backend ID picked by the dashboard ("" = router default)
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
func (h *Hub) sessionSend(p sendParams, onAsyncError func(string)) (bool, sendAckStatus, error) {
	key := p.Key
	// R175-SEC-P1: use the canonical session.ValidateSessionKey gate
	// (also used by handleEvents / handleDelete / handleSetLabel / HTTP
	// handleInterrupt / WS handleSubscribe / WS handleInterrupt). The
	// previous inline loop only rejected ASCII C0 / DEL; C1 controls
	// (U+0080-U+009F), bidi overrides (U+202A-U+202E / U+2066-U+2069),
	// and non-UTF-8 sequences fell through and reached slog attrs +
	// sessions.json, giving an authenticated caller a log-injection
	// primitive. ValidateSessionKey also caps at MaxSessionKeyBytes
	// (~520 B) which supersedes the old local 512 ceiling.
	if err := session.ValidateSessionKey(key); err != nil {
		return false, "", fmt.Errorf("invalid key")
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
			// Decouple the client-facing message from the internal error
			// chain so any future edit wrapping an os.PathError with %w in
			// validateWorkspace cannot leak the resolved filesystem path to
			// the dashboard user. Full detail stays in the operator log.
			// R58-SEC-L2.
			//
			// Log at Warn, not Debug — validateWorkspace rejects path
			// traversal, symlink escapes, and out-of-root roots, which are
			// security-relevant events operators should see without flipping
			// on verbose logging. The workspace path is already scrubbed
			// from the HTTP response, so surfacing it in logs doesn't leak
			// it to the client. R59-GO-M2.
			//
			// R175-SEC-P1: p.Workspace is attacker-influenced (authenticated
			// dashboard/node, but not operator-supplied). The ValidateSessionKey
			// gate above rejects C1/bidi in the KEY; workspace is validated
			// separately as a filesystem path and passes through here raw.
			// Route it through osutil.SanitizeForLog so C1 controls / bidi
			// overrides / LS/PS cannot flip terminal rendering under `tail
			// -f` or inject fake lines into JSON log sinks.
			slog.Warn("workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(p.Workspace, 1024))
			return false, "", fmt.Errorf("invalid workspace")
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

	// Dashboard-picked backend override. Recorded per key so spawnSession
	// (which runs later inside runTurn, not here) can pick up the choice
	// when it actually fires up a wrapper. Unknown IDs are clamped to the
	// router default inside wrapperFor, but we reject obviously hostile
	// input early so a 4 KB `backend=<payload>` cannot land in logs.
	if p.Backend != "" {
		if len(p.Backend) > 32 {
			return false, "", fmt.Errorf("invalid backend length")
		}
		for i := 0; i < len(p.Backend); i++ {
			c := p.Backend[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				return false, "", fmt.Errorf("invalid backend character")
			}
		}
		h.router.SetSessionBackend(key, p.Backend)
	}

	// Resume registration — bound length before regex scan to limit cost
	// of a hostile multi-MB resume_id (declared but unvalidated elsewhere).
	// Valid session IDs are UUIDs (36 chars); 64 gives headroom for future formats.
	if len(p.ResumeID) > 64 {
		return false, "", fmt.Errorf("invalid resume_id length")
	}
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

	// Passthrough mode: direct dispatch — every send gets its own goroutine
	// and runs concurrently with any other in-flight send for the same
	// session. The CLI's commandQueue + Process-level sendSlot FIFO handle
	// ordering. If the session's protocol doesn't support replay,
	// usePassthrough() inside sendWithBroadcast transparently falls back
	// to the legacy serialized Send path.
	if h.queue.Mode() == dispatch.ModePassthrough {
		release, shuttingDown := h.TrackSend()
		if shuttingDown {
			return false, sendAckBusy, nil
		}
		// /urgent prefix → strip + set priority:"now" so the CLI aborts
		// the in-flight turn. Keeps dashboard behavior parallel with the IM
		// dispatcher's /urgent command (dispatch/commands.go handleUrgent).
		text := p.Text
		priority := ""
		if strings.HasPrefix(strings.TrimSpace(text), "/urgent ") {
			text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/urgent "))
			priority = "now"
		}
		go func() {
			defer release()
			h.runTurnPassthrough(p.Key, text, p.Images, priority, onAsyncError)
		}()
		return false, sendAckAccepted, nil
	}

	qm := dispatch.QueuedMsg{
		Text:      p.Text,
		Images:    p.Images,
		EnqueueAt: time.Now(),
	}
	isOwner, enqueued, shouldInterrupt, gen := h.queue.Enqueue(key, qm)
	if !isOwner {
		if shouldInterrupt {
			// Interrupt mode: abort the in-flight turn so the queued
			// follow-up can be processed promptly. See dispatch.go for the
			// full rationale; this mirrors the IM path. Non-Sent outcomes
			// degrade silently to Collect (the message is still queued).
			switch outcome := h.router.InterruptSessionViaControl(key); outcome {
			case session.InterruptSent:
				slog.Debug("send: aborted active turn to process follow-up", "key", key)
			case session.InterruptNoTurn:
				slog.Debug("send: session idle or spawning, will process follow-up after current turn", "key", key)
			case session.InterruptNoSession:
				slog.Debug("send: session not found, falling back to collect", "key", key)
			case session.InterruptUnsupported:
				slog.Debug("send: protocol does not support stdin interrupt, falling back to collect", "key", key)
			case session.InterruptError:
				slog.Warn("send: transport error during interrupt, falling back to collect", "key", key)
			}
		}
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
			h.handleOwnerLoopPanic(key, onAsyncError, r)
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

// handleOwnerLoopPanic is the deferred panic recovery helper for ownerLoop.
// Split out of the defer so the recover path can be unit-tested directly —
// constructing a panicking runTurn in tests would require a real router +
// session, which is out of scope for a targeted recover regression. The
// helper:
//
//  1. Logs the panic with a full stack trace for operator triage.
//  2. Clears the message queue so a stale owner is not left holding the key.
//  3. Signals the dashboard client via onAsyncError so the UI can tell the
//     user the turn was lost. HTTP path passes nil onAsyncError (ack already
//     shipped), so this is a no-op there. RETRY3.
//
// A nested recover around onAsyncError absorbs a cascading panic (e.g., a
// broken WS writer) so the outer defer always completes.
func (h *Hub) handleOwnerLoopPanic(key string, onAsyncError func(string), r any) {
	slog.Error("ownerLoop panic", "key", key, "panic", r, "stack", string(debug.Stack()))
	if h.queue != nil {
		h.queue.Discard(key)
	}
	if onAsyncError != nil {
		func() {
			defer func() { _ = recover() }()
			onAsyncError("处理异常，请刷新页面或稍后重试。")
		}()
	}
}

// sessionOptsFor returns the AgentOpts to use when spawning (or resuming)
// the session for key.
//
// Scratch (ephemeral aside) keys are resolved via the pool so the inherited
// agent/backend/model/workspace config plus the --append-system-prompt quote
// shim land on the spawned CLI. Every other key falls back to
// buildSessionOpts, which consults the agent registry and planner overrides.
//
// The pool lookup touches the scratch's lastUsed timestamp so an actively
// used aside is not swept out from under the user. This "Touch on lookup"
// pattern is the only mechanism preventing the sweeper from evicting a
// scratch that is about to receive its first send — do not remove it.
func (h *Hub) sessionOptsFor(key string) session.AgentOpts {
	if h.scratchPool != nil && session.IsScratchKey(key) {
		if opts, ok := h.scratchPool.OptsForKey(key); ok {
			return opts
		}
	}
	return buildSessionOpts(key, h.agents, h.projectMgr)
}

// runTurn executes one send turn: GetOrCreate + sendWithBroadcast.
func (h *Hub) runTurn(key, text string, images []cli.ImageData, onAsyncError func(string)) {
	sendStart := time.Now()
	opts := h.sessionOptsFor(key)
	sess, status, err := h.router.GetOrCreate(h.ctx, key, opts)
	if err != nil {
		slog.Error("send: get session", "key", key, "err", err)
		if onAsyncError != nil {
			onAsyncError(err.Error())
		}
		return
	}
	if status != session.SessionExisting {
		// Debug (not Info): router.spawnSession emits "session spawned" at
		// Info with key + active count for every spawn regardless of caller;
		// surfacing the send-layer spawn row additionally just doubles the
		// journal entries per spawn. Keep the elapsed_ms detail at Debug
		// so it is still available when operators opt into verbose logging
		// (e.g. investigating slow spawn paths).
		slog.Debug("send: session spawned", "key", key, "status", status, "elapsed_ms", time.Since(sendStart).Milliseconds())
	}

	if _, err := h.sendWithBroadcast(h.ctx, key, sess, text, images, nil); err != nil {
		slog.Error("send: send", "key", key, "err", err)
	} else if h.scheduler != nil && session.IsCronKey(key) {
		if err := h.scheduler.SetJobPrompt(strings.TrimPrefix(key, session.CronKeyPrefix), text); err != nil {
			slog.Warn("send: set cron prompt", "key", key, "err", err)
		}
	}
	slog.Debug("send: turn complete", "key", key, "elapsed_ms", time.Since(sendStart).Milliseconds())
}

// runTurnPassthrough runs one passthrough-mode turn. Called from a detached
// goroutine so multiple sends on the same session execute concurrently. The
// session layer routes through SendPassthrough when the protocol supports
// replay; otherwise it transparently falls back to the legacy serialized
// Send path (matching dispatch's fallback semantics).
//
// `priority` is forwarded as-is to SendPassthrough: "" for normal messages,
// "now" for /urgent preemption.
func (h *Hub) runTurnPassthrough(key, text string, images []cli.ImageData, priority string, onAsyncError func(string)) {
	sendStart := time.Now()
	opts := h.sessionOptsFor(key)
	sess, _, err := h.router.GetOrCreate(h.ctx, key, opts)
	if err != nil {
		slog.Error("passthrough: get session", "key", key, "err", err)
		if onAsyncError != nil {
			onAsyncError(err.Error())
		}
		return
	}
	ctx := dispatch.WithPassthrough(h.ctx)
	if _, err := h.sendWithBroadcastPriority(ctx, key, sess, text, images, nil, priority); err != nil {
		// ErrAbortedByUrgent, ErrReconnectedUnknown, ErrSessionReset are
		// informational — the user knows what happened (or will see a
		// dashboard state update). Only log at Warn for surprising failures.
		if errors.Is(err, cli.ErrAbortedByUrgent) ||
			errors.Is(err, cli.ErrSessionReset) ||
			errors.Is(err, cli.ErrReconnectedUnknown) {
			slog.Debug("passthrough: send completed with informational error", "key", key, "err", err)
		} else {
			slog.Warn("passthrough: send failed", "key", key, "err", err)
		}
		if onAsyncError != nil {
			onAsyncError(err.Error())
		}
	} else if h.scheduler != nil && session.IsCronKey(key) {
		if err := h.scheduler.SetJobPrompt(strings.TrimPrefix(key, session.CronKeyPrefix), text); err != nil {
			slog.Warn("passthrough: set cron prompt", "key", key, "err", err)
		}
	}
	slog.Debug("passthrough: turn complete", "key", key, "elapsed_ms", time.Since(sendStart).Milliseconds())
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
