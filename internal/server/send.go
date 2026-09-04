// send.go contains sendWithBroadcast, the canonical wrapper for sending
// messages to a session with dashboard state notifications. Every entry point
// that sends user messages (IM, HTTP API, WebSocket) must use it rather than
// sess.Send so the dashboard sees running/ready transitions; cron is the only
// exception (own notification path via BroadcastCronResult).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// sendWithBroadcast wraps sess.Send with dashboard state broadcasts ("running"
// before, session snapshot after); Server.sendWithBroadcast delegates here.
// sess must be non-nil; callers must check the error from GetOrCreate first.
func (h *Hub) sendWithBroadcast(
	ctx context.Context,
	key string,
	sess *session.ManagedSession,
	text string,
	images []cli.Attachment,
	onEvent cli.EventCallback,
) (*cli.SendResult, error) {
	return h.sendWithBroadcastPriority(ctx, key, sess, text, images, onEvent, "")
}

// sendWithBroadcastPriority is the passthrough-aware variant of
// sendWithBroadcast: when the ctx asks for passthrough and the session
// supports it, the turn goes through SendPassthrough so concurrent sends on
// one session can overlap; otherwise the serialized Send path is used. A ctx
// carrying dispatch.WithUrgent upgrades an unset priority to "now".
func (h *Hub) sendWithBroadcastPriority(
	ctx context.Context,
	key string,
	sess *session.ManagedSession,
	text string,
	images []cli.Attachment,
	onEvent cli.EventCallback,
	priority string,
) (*cli.SendResult, error) {
	// Only the running-state transition here; the post-send (debounced)
	// BroadcastSessionsUpdate covers the sessions snapshot.
	h.BroadcastSessionReady(key)

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
		// turn first. Best-effort — the message still lands on the next turn.
		sess.InterruptViaControl()
		result, err = sess.Send(ctx, text, images, onEvent)
	default:
		result, err = sess.Send(ctx, text, images, onEvent)
	}

	if rs := h.router.SessionFor(key); rs != nil {
		snap := rs.Snapshot()
		h.broadcastState(key, snap.State, snap.DeathReason)
	}
	h.BroadcastSessionsUpdate()

	return result, err
}

// usePassthrough reports whether this turn should take the passthrough path.
func usePassthrough(ctx context.Context, sess *session.ManagedSession) bool {
	if sess == nil || !sess.SupportsPassthrough() {
		return false
	}
	return dispatch.IsPassthrough(ctx)
}

// sendWithBroadcast delegates to Hub.sendWithBroadcast when a dashboard Hub is
// wired. Without a hub it falls back to a direct, broadcast-free sess.Send
// only for Headless Servers; a non-headless Server with a nil hub is a wiring
// regression and panics rather than silently dropping every broadcast (#379).
//
// sess must be non-nil; callers must check the error from GetOrCreate first.
func (s *Server) sendWithBroadcast(
	ctx context.Context,
	key string,
	sess *session.ManagedSession,
	text string,
	images []cli.Attachment,
	onEvent cli.EventCallback,
) (*cli.SendResult, error) {
	if sess == nil {
		return nil, fmt.Errorf("sendWithBroadcast: session is nil")
	}
	if s.hub != nil {
		return s.hub.sendWithBroadcast(ctx, key, sess, text, images, onEvent)
	}
	if !s.headless {
		// Wiring regression — fail loud instead of silently dropping broadcasts.
		panic("server: sendWithBroadcast called with nil hub on a non-headless Server (set ServerOptions.Headless for hub-less wiring)")
	}
	// Headless (no hub): still honour passthrough when requested and supported.
	if usePassthrough(ctx, sess) {
		return sess.SendPassthrough(ctx, text, images, onEvent, "")
	}
	return sess.Send(ctx, text, images, onEvent)
}

// sendParams holds parsed input for a session send request (HTTP and WebSocket).
type sendParams struct {
	Key       string
	Text      string
	Images    []cli.Attachment
	Workspace string
	ResumeID  string
	Backend   string // optional backend ID picked by the dashboard ("" = router default)
	// AccessProfile is the optional access-profile ID picked by the dashboard
	// new-session picker ("" = global default). One-shot: recorded per key and
	// consumed by spawnSession. RFC project-access-profile §8.2.
	AccessProfile string
}

// sendAckStatus describes the immediate ack status for a queued send.
//   - "accepted": caller became the owner; message is processing now.
//   - "queued":   session was busy; message is queued behind the active turn.
type sendAckStatus string

const (
	sendAckAccepted sendAckStatus = "accepted"
	sendAckQueued   sendAckStatus = "queued"
	// sendAckBusy: session busy and queue disabled (MaxDepth<=0), so the
	// message was not buffered — the client should retry.
	sendAckBusy sendAckStatus = "busy"
)

// interruptAcquireTimeout bounds how long the post-interrupt AcquireTimeout
// waits for the previous owner to release the per-key guard.
const interruptAcquireTimeout = 2 * time.Second

// sessionSend validates and dispatches a send request. Returns (true, "", nil)
// for a /clear or /new reset; (false, "", err) on validation failure;
// (false, "accepted", nil) when we own the turn; (false, "queued", nil) when
// the message was enqueued for the owner's drain loop to coalesce.
// onAsyncError (may be nil) fires from the owner goroutine when the turn fails
// after the ack, with the underlying error (nil at literal-message sites) +
// localised label so fan-out callers can filter (Hub.httpSendErrorCallback).
func (h *Hub) sessionSend(p sendParams, onAsyncError asyncErrorFn) (bool, sendAckStatus, error) {
	key := p.Key
	// ValidateSessionKey rejects C0/C1 controls, bidi overrides, non-UTF-8 and
	// over-long keys: no log-injection primitive via slog / sessions.json.
	if err := session.ValidateSessionKey(key); err != nil {
		return false, "", fmt.Errorf("invalid key")
	}

	// /clear and /new — the CLI built-in doesn't work in stream-json; also
	// drop the pending queue. Case-insensitive so CJK mobile IMEs that
	// auto-capitalize ("/Clear") still reset (as dispatch.normalizeSlashCommand).
	trimmed := strings.ToLower(strings.TrimSpace(p.Text))
	if trimmed == "/clear" || trimmed == "/new" {
		if h.queue != nil {
			h.queue.Discard(key)
		}
		// queue.Discard 只清排队消息；in-flight 的 SendPassthrough goroutine
		// 也要通知到，否则它们继续占着 sendSlot 直到超时，新消息被
		// ErrTooManyPending 拒绝（与 IM 路径 dispatch.discardQueue 对齐）。
		if sess := h.router.SessionFor(key); sess != nil {
			sess.DiscardPassthroughPending(cli.ErrSessionReset)
		}
		// Atomic Reset + workspaceOverride delete: a concurrent SetWorkspace
		// must not survive and leak into the fresh session.
		h.router.ResetAndDiscardOverride(key)
		h.BroadcastSessionsUpdate()
		return true, "", nil
	}

	var validatedWorkspace string
	if p.Workspace != "" {
		wsPath, err := validateWorkspace(p.Workspace, h.allowedRoot)
		if err != nil {
			// Generic client message: the error chain may embed the resolved
			// path. Warn — rejects are traversal / symlink-escape events.
			// p.Workspace is attacker-influenced, so SanitizeForLog (200-byte
			// cap, same as other attacker-influenced fields).
			slog.Warn("workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(p.Workspace, 200))
			return false, "", fmt.Errorf("invalid workspace")
		}
		validatedWorkspace = wsPath
		// Refuse an empty chat-key prefix (":agentID"): it would persist "" as
		// the override for every GetWorkspace("") lookup.
		if idx := strings.LastIndexByte(key, ':'); idx > 0 {
			h.router.SetWorkspace(key[:idx], wsPath)
		}
	}

	// Dashboard-picked backend override, recorded per key and consumed by
	// spawnSession inside runTurn. Unknown IDs clamp to the router default in
	// wrapperFor; only hostile input is rejected here (keeps it out of logs).
	if p.Backend != "" {
		// Shared isValidBackendID / maxBackendIDLen so HTTP, WS dispatch and
		// node selection accept the same IDs; error text aligned with
		// dashboard_cron.validateCronBackend for dashboard JS substring matching.
		if len(p.Backend) > maxBackendIDLen {
			return false, "", fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
		}
		if !isValidBackendID(p.Backend) {
			return false, "", fmt.Errorf("invalid backend identifier")
		}
		h.router.SetSessionBackend(key, p.Backend)
	}

	// Dashboard-picked access-profile override (RFC project-access-profile
	// §8.2); same one-shot semantics as Backend. Unknown IDs resolve to the
	// global default in resolveSpawnParamsLocked; only hostile input is rejected.
	if p.AccessProfile != "" {
		if len(p.AccessProfile) > maxBackendIDLen || !isValidBackendID(p.AccessProfile) {
			return false, "", fmt.Errorf("invalid access_profile identifier")
		}
		h.router.SetSessionAccessProfile(key, p.AccessProfile)
	}

	// Bound resume_id length before the regex scan (UUIDs are 36 chars; 64
	// leaves headroom) so a hostile multi-MB value costs nothing.
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

	// Legacy guard path when no queue is configured (tests, headless);
	// legacySendInvokes lets migrators observe remaining fixtures (#710).
	if h.queue == nil {
		h.legacySendInvokes.Add(1)
		return h.sessionSendLegacy(p, onAsyncError)
	}

	// Passthrough mode: every send gets its own goroutine; the CLI's
	// commandQueue + sendSlot FIFO handle ordering. Protocols without replay
	// fall back to serialized Send inside sendWithBroadcast (usePassthrough).
	if h.queue.Mode() == dispatch.ModePassthrough {
		release, shuttingDown := h.TrackSend()
		if shuttingDown {
			return false, sendAckBusy, nil
		}
		// /urgent prefix → strip + priority "now" so the CLI aborts the
		// in-flight turn (parallels dispatch/commands.go handleUrgent).
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
	isOwner, enqueued, shouldInterrupt, gen, _ := h.queue.Enqueue(key, qm)
	if !isOwner {
		if shouldInterrupt {
			// Interrupt mode: abort the in-flight turn so the queued follow-up
			// runs promptly (mirrors dispatch.go). Non-Sent outcomes degrade to Collect.
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
			// Queue disabled (MaxDepth<=0) and session busy — the message is
			// dropped; tell the client to retry.
			slog.Debug("send: message dropped (session busy, queue disabled)", "key", key)
			return false, sendAckBusy, nil
		}
		// Queued; the owner's ownerLoop picks it up on its next drain tick.
		slog.Debug("send: message queued (session busy)", "key", key)
		return false, sendAckQueued, nil
	}

	// Owner — spawn the drain loop. TrackSend declines a send arriving
	// concurrently with Shutdown instead of escaping past sendWG.Wait.
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		// Discard drops ownership (bumps gen, clears the owner flag) so a
		// later Enqueue can re-own.
		h.queue.Discard(key)
		return false, sendAckBusy, nil
	}
	go func() {
		defer release()
		h.ownerLoop(key, gen, qm, onAsyncError)
	}()
	return false, sendAckAccepted, nil
}

// sessionOptsFor returns the AgentOpts to use when spawning (or resuming)
// the session for key: scratch keys via the pool (inherited config + the
// --append-system-prompt quote shim), everything else via buildSessionOpts.
// The pool lookup touches the scratch's lastUsed — that "Touch on lookup" is
// the only thing preventing the sweeper from evicting a scratch about to
// receive its first send. Do not remove it.
func (h *Hub) sessionOptsFor(key string) session.AgentOpts {
	if h.scratchPool != nil && sessionkey.IsScratchKey(key) {
		if opts, ok := h.scratchPool.OptsForKey(key); ok {
			return opts
		}
	}
	return buildSessionOpts(key, h.resolver, h.agents, h.projectMgr)
}

// runTurn executes one send turn: GetOrCreate + sendWithBroadcast.
func (h *Hub) runTurn(key, text string, images []cli.Attachment, onAsyncError asyncErrorFn) {
	sendStart := time.Now()
	opts := h.sessionOptsFor(key)
	sess, status, err := h.router.GetOrCreate(h.ctx, key, opts)
	if err != nil {
		slog.Error("send: get session", "key", key, "err", err)
		if onAsyncError != nil {
			onAsyncError(err, asyncErrorMessage(err))
		}
		return
	}
	if status != session.SessionExisting {
		// Debug, not Info: router.spawnSession already logs "session spawned"
		// at Info for every spawn.
		slog.Debug("send: session spawned", "key", key, "status", status, "elapsed_ms", time.Since(sendStart).Milliseconds())
	}

	if _, err := h.sendWithBroadcast(h.ctx, key, sess, text, images, nil); err != nil {
		slog.Error("send: send", "key", key, "err", err)
	} else {
		h.autoSaveCronPrompt("send", key, text)
	}
	slog.Debug("send: turn complete", "key", key, "elapsed_ms", time.Since(sendStart).Milliseconds())
}

// runTurnPassthrough runs one passthrough-mode turn from a detached goroutine
// so sends on the same session overlap; protocols without replay fall back to
// serialized Send. priority is "" or "now" (/urgent preemption).
func (h *Hub) runTurnPassthrough(key, text string, images []cli.Attachment, priority string, onAsyncError asyncErrorFn) {
	sendStart := time.Now()
	opts := h.sessionOptsFor(key)
	sess, _, err := h.router.GetOrCreate(h.ctx, key, opts)
	if err != nil {
		slog.Error("passthrough: get session", "key", key, "err", err)
		if onAsyncError != nil {
			onAsyncError(err, asyncErrorMessage(err))
		}
		return
	}
	ctx := dispatch.WithPassthrough(h.ctx)
	if _, err := h.sendWithBroadcastPriority(ctx, key, sess, text, images, nil, priority); err != nil {
		// ErrAbortedByUrgent / ErrReconnectedUnknown / ErrSessionReset are
		// informational; only surprising failures log at Warn.
		if informationalSendErr(err) {
			slog.Debug("passthrough: send completed with informational error", "key", key, "err", err)
		} else {
			slog.Warn("passthrough: send failed", "key", key, "err", err)
		}
		if onAsyncError != nil {
			onAsyncError(err, asyncErrorMessage(err))
		}
	} else {
		h.autoSaveCronPrompt("passthrough", key, text)
	}
	slog.Debug("passthrough: turn complete", "key", key, "elapsed_ms", time.Since(sendStart).Milliseconds())
}

// autoSaveCronPrompt persists the just-sent text as the cron job's prompt on
// a successful turn; no-op for non-cron keys or without a scheduler.
// ErrPromptAlreadySet (every turn after the first) is benign and not logged.
func (h *Hub) autoSaveCronPrompt(phase, key, text string) {
	if h.scheduler == nil || !sessionkey.IsCronKey(key) {
		return
	}
	jobID := strings.TrimPrefix(key, sessionkey.CronKeyPrefix)
	if err := h.scheduler.SetJobPrompt(jobID, text); err != nil && !errors.Is(err, cron.ErrPromptAlreadySet) {
		slog.Warn(phase+": set cron prompt", "key", key, "err", err)
	}
}

// Deprecated: sessionSend with a configured MessageQueue handles all production
// paths. sessionSendLegacy keeps the pre-queue guard/interrupt behaviour only
// for tests that do not wire a MessageQueue. Removal tracked in docs/TODO.md
// R-LEGACY-SEND: delete it with its sole caller branch once every test wires one.
func (h *Hub) sessionSendLegacy(p sendParams, onAsyncError asyncErrorFn) (bool, sendAckStatus, error) {
	key := p.Key

	acquired := h.guard.TryAcquire(key)
	needInterrupt := !acquired
	if needInterrupt {
		// InterruptSessionSafe (control_request → SIGINT fallback): raw SIGINT
		// kills Claude `-p` outright, burning a shim slot and resume context.
		h.router.InterruptSessionSafe(key)
		slog.Debug("send: interrupted running session", "key", key)
	}

	text, images := p.Text, p.Images
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		if !needInterrupt {
			// Acquired but not spawning — release so a later enqueue can
			// re-acquire (needInterrupt=true means we never acquired).
			h.guard.Release(key)
		}
		return false, sendAckBusy, nil
	}
	go func() {
		defer release()
		if needInterrupt {
			// AcquireTimeout writes a Guard.lastWait entry cleared only by
			// Release; the defer Release below covers this site.
			if !h.guard.AcquireTimeout(h.ctx, key, interruptAcquireTimeout) {
				slog.Error("send: interrupt timed out", "key", key)
				if onAsyncError != nil {
					onAsyncError(nil, "会话中断超时，请稍后重试。")
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
