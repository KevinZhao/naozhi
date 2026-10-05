// relay_origin.go — the entry for a send a primary relays to this node over
// the reverse link (the upstream connector's "send" RPC). It runs on the same
// turn.Orchestrator as IM and the dashboard: /new and /clear reset, /urgent
// preempts, sends on a busy key coalesce, and agent keys spawn with their
// agent's options.
package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// relayFailPrefix heads the system event a failed relayed send leaves in the
// session's EventLog.
const relayFailPrefix = "发送失败："

var (
	errSendBusy          = errors.New("会话正忙，消息未送达，请稍后重试")
	errRelayShuttingDown = errors.New("节点正在关闭，消息未送达")
)

// SubmitRelayed runs a send a primary relayed over the reverse link; it is
// the upstream connector's TurnSubmitter. workspace has already passed the
// connector's own checks. Returns "reset", "accepted" or "queued".
func (s *Server) SubmitRelayed(ctx context.Context, key, text, workspace string) (string, error) {
	return s.hub.submitRelayed(ctx, key, text, workspace)
}

// relaySend is sessionSend for a relayed send. The session is created on ctx
// before Submit, so it exists when the RPC answers and the primary's
// follow-up subscribe finds it; a spawn failure is the RPC's error. A send
// that was not buffered is an error too: it is the only answer the primary
// passes on.
func (e *sendEngine) relaySend(ctx context.Context, key, text, workspace string) (string, error) {
	p := sendParams{Key: key, Text: text, Workspace: workspace}
	cmd, reset, err := e.prepareSend(p)
	if err != nil {
		return "", err
	}
	if reset {
		return string(sendAckReset), nil
	}
	if _, _, err := e.router.GetOrCreate(ctx, key, e.sessionOptsFor(key)); err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}
	origin := &relayOrigin{dashOrigin: dashOrigin{key: key, opts: e.sessionOptsFor}, sessions: e.router}
	switch e.submit(p, cmd, origin) {
	case turn.AckOwner, turn.AckDetached:
		return string(sendAckAccepted), nil
	case turn.AckQueued:
		return string(sendAckQueued), nil
	case turn.AckDropped:
		return "", errSendBusy
	default:
		return "", errRelayShuttingDown
	}
}

// relayOrigin is a relayed send. Like httpOrigin, one receiver speaks for
// every relayed send on the key in the batch, and informational outcomes are
// dropped. A failure goes into the session's EventLog: the primary's tabs
// follow this node's sessions through it, and so do this node's own. A turn
// cut short by the node shutting down is not reported: the shim may carry
// it through the restart, and a persisted "retry" would invite a resend.
type relayOrigin struct {
	dashOrigin
	sessions interface {
		SessionFor(key string) *session.ManagedSession
	}
}

func (o *relayOrigin) Sink() string { return "relay:" + o.key }

func (o *relayOrigin) Begin(_ context.Context, t turn.TurnInfo) turn.Delivery {
	if t.Role == turn.RoleObserver {
		return nil
	}
	return o
}

func (o *relayOrigin) Finish(ctx context.Context, out turn.Outcome) {
	// ctx is the engine's, cancelled by drain only.
	if msg, failed, err := o.failure(out); failed && !informationalSendErr(err) && ctx.Err() == nil {
		o.report(msg)
	}
}

// Dropped reports the failures a queued relayed send can meet; a reset or a
// shutdown speaks for itself through the session's state. DropRemoved is not
// reported: the retired key has no session left to log into.
func (o *relayOrigin) Dropped(_ context.Context, why turn.DropReason) {
	switch why {
	case turn.DropEvicted:
		o.report(evictedSendMsg)
	case turn.DropPanic:
		o.report(turnPanicMsg)
	}
}

// report writes msg into the key's EventLog, sanitized: it is persisted and
// broadcast to every subscriber, on this node and through the primary.
func (o *relayOrigin) report(msg string) {
	if sess := o.sessions.SessionFor(o.key); sess != nil {
		sess.LogSystemEvent(relayFailPrefix + osutil.SanitizeForLog(msg, 512))
	}
}
