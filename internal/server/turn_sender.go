// turn_sender.go — turnSender is turn.Orchestrator's session side
// (turn.Sender) for every entry, IM and dashboard: it creates and sends on
// the router's sessions and tells the dashboard about each turn. It holds the
// router and the broadcaster, never the Hub (send_engine_ownership check A).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
)

// turnRouter is the *session.Router surface turnSender uses.
type turnRouter interface {
	GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
	SessionFor(key string) *session.ManagedSession
	InterruptSessionViaControl(key string) session.InterruptOutcome
	DiscardPassthroughPending(key string, reason error)
	Reset(key string)
	ResetAndDiscardOverride(key string)
	NotifyIdle()
}

var _ turn.Sender = turnSender{}

type turnSender struct {
	router turnRouter
	notify sendNotifier
	// prompts saves a cron session's first successful prompt; nil without
	// a scheduler.
	prompts cronPromptSaver
}

// cronPromptSaver is the scheduler surface turnSender uses.
type cronPromptSaver interface {
	SetJobPrompt(jobID, prompt string) error
}

// GetOrCreate returns a missing session as a nil interface: a nil
// *ManagedSession inside a non-nil turn.Session would reach Send.
func (s turnSender) GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (turn.Session, session.SessionStatus, error) {
	start := time.Now()
	sess, status, err := s.router.GetOrCreate(ctx, key, opts)
	if err == nil && status != session.SessionExisting {
		// Debug, not Info: router.spawnSession already logs "session spawned"
		// at Info for every spawn.
		slog.Debug("send: session spawned", "key", key, "status", status, "elapsed_ms", time.Since(start).Milliseconds())
	}
	if sess == nil {
		return nil, status, err
	}
	return sess, status, err
}

// Send runs the turn on the session GetOrCreate produced; anything else is a
// wiring fault, reported rather than dereferenced. spec.Passthrough takes the
// concurrent path only when the session supports it.
func (s turnSender) Send(ctx context.Context, key string, ts turn.Session, text string, images []clievent.Attachment, spec turn.SendSpec, onEvent clievent.EventCallback) (*clievent.SendResult, error) {
	sess, ok := ts.(*session.ManagedSession)
	if !ok || sess == nil {
		return nil, fmt.Errorf("server: turn sent on a session turnSender did not produce (%T)", ts)
	}
	priority := ""
	if spec.Priority == turn.PriorityNow {
		priority = "now"
	}
	start := time.Now()
	passthrough := spec.Passthrough && sess.SupportsPassthrough()
	result, err := sendTurn(ctx, s.notify, key, sess, text, images, onEvent, passthrough, priority)
	if err == nil {
		s.autoSaveCronPrompt(key, text)
	}
	slog.Debug("send: turn complete", "key", key, "elapsed_ms", time.Since(start).Milliseconds())
	return result, err
}

// autoSaveCronPrompt persists a successful turn's text as the cron job's
// prompt; a no-op for non-cron keys or without a scheduler.
// ErrPromptAlreadySet (every turn after the first) is benign and not logged.
func (s turnSender) autoSaveCronPrompt(key, text string) {
	if s.prompts == nil || !sessionkey.IsCronKey(key) {
		return
	}
	jobID := strings.TrimPrefix(key, sessionkey.CronKeyPrefix)
	if err := s.prompts.SetJobPrompt(jobID, text); err != nil && !errors.Is(err, cron.ErrPromptAlreadySet) {
		slog.Warn("send: set cron prompt", "key", key, "err", err)
	}
}

func (s turnSender) AfterTurn(key string) { broadcastAfterTurn(s.router, s.notify, key) }

func (s turnSender) Interrupt(key string) session.InterruptOutcome {
	return s.router.InterruptSessionViaControl(key)
}

func (s turnSender) DiscardPending(key string, reason error) {
	s.router.DiscardPassthroughPending(key, reason)
}

// Reset resets key's session; discardOverride also drops the chat's
// workspace override in the same step.
func (s turnSender) Reset(key string, discardOverride bool) {
	if discardOverride {
		s.router.ResetAndDiscardOverride(key)
		return
	}
	s.router.Reset(key)
}

func (s turnSender) NotifyIdle() { s.router.NotifyIdle() }

// sendTurn broadcasts that key is running, then sends: through
// SendPassthrough when passthrough is set (so sends on one session can
// overlap), else through the serialized Send. priority "now" without
// passthrough interrupts the in-flight turn first — best-effort, the message
// still lands on the next turn.
func sendTurn(ctx context.Context, notify sendNotifier, key string, sess *session.ManagedSession, text string, images []clievent.Attachment, onEvent clievent.EventCallback, passthrough bool, priority string) (*clievent.SendResult, error) {
	// Only the running-state transition here; broadcastAfterTurn's
	// (debounced) BroadcastSessionsUpdate covers the sessions snapshot.
	notify.BroadcastSessionReady(key)
	switch {
	case passthrough:
		return sess.SendPassthrough(ctx, text, images, onEvent, priority)
	case priority == "now":
		sess.InterruptViaControl()
		return sess.Send(ctx, text, images, onEvent)
	default:
		return sess.Send(ctx, text, images, onEvent)
	}
}

// broadcastAfterTurn pushes key's post-turn state and the sessions snapshot
// to the dashboard.
func broadcastAfterTurn(router interface {
	SessionFor(key string) *session.ManagedSession
}, notify sendNotifier, key string) {
	if rs := router.SessionFor(key); rs != nil {
		snap := rs.Snapshot()
		notify.broadcastState(key, snap.State, snap.DeathReason)
	}
	notify.BroadcastSessionsUpdate()
}
