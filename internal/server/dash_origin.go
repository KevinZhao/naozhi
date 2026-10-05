// dash_origin.go — the dashboard's turn.Origin kinds. A WS send is its own
// receiver (one sink per send id) and hears about its turn as an error
// send_ack to that id. HTTP sends share one receiver per key: a failed turn is
// one send_error broadcast to the key's subscribers, however many HTTP
// requests it carried (#3004 分叉 22). Neither is Blocking, so the error goes
// out before the post-turn state broadcast (分叉 21), and neither says
// anything as an Observer of a batch it is not in.
package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// turnPanicMsg is what a request whose turn panicked is told.
const turnPanicMsg = "处理异常，请稍后重试。"

// evictedSendMsg is what a WS send pushed out of a full queue is told.
const evictedSendMsg = "排队消息过多，这条消息已被丢弃，请重新发送。"

// removedSendMsg is what a WS send queued on a key the router retired is told.
const removedSendMsg = "会话已结束，这条消息未被处理，请重新发送。"

// dashOrigin is what both dashboard origins share. opts is the engine's
// sessionOptsFor, asked once per turn the origin owns (#3004 分叉 26).
// Admission is acked by the transport with sessionSend's status, so
// Admitted does nothing; the Delivery hooks before Finish are no-ops.
type dashOrigin struct {
	key  string
	opts func(key string) session.AgentOpts
}

func (o dashOrigin) SessionOpts(key string) session.AgentOpts { return o.opts(key) }
func (dashOrigin) Admitted(context.Context, turn.Ack)         {}
func (dashOrigin) BeforeSession(context.Context)              {}
func (dashOrigin) Blocking() bool                             { return false }

func (dashOrigin) SessionReady(context.Context, session.SessionStatus) clievent.EventCallback {
	return nil
}

// failure returns out's user-facing label and error, or failed=false for a
// turn that succeeded.
func (o dashOrigin) failure(out turn.Outcome) (msg string, failed bool, err error) {
	switch {
	case out.Panic:
		return turnPanicMsg, true, nil
	case out.Stage == turn.StageDone:
		return "", false, nil
	}
	if informationalSendErr(out.Err) {
		slog.Debug("dashboard turn ended with an informational error", "key", o.key, "err", out.Err)
	} else {
		slog.Warn("dashboard turn failed", "key", o.key, "stage", out.Stage, "err", out.Err)
	}
	return asyncErrorMessage(out.Err), true, out.Err
}

// wsOrigin is one WebSocket send. Its failures, informational ones included,
// go to the sending tab alone, which is why they are not filtered.
type wsOrigin struct {
	dashOrigin
	c  *wsClient
	id string
}

func (o *wsOrigin) Sink() string { return fmt.Sprintf("ws:%p:%s", o.c, o.id) }

func (o *wsOrigin) Begin(_ context.Context, t turn.TurnInfo) turn.Delivery {
	if t.Role == turn.RoleObserver {
		return nil
	}
	return o
}

func (o *wsOrigin) Finish(_ context.Context, out turn.Outcome) {
	if msg, failed, _ := o.failure(out); failed {
		o.fail(msg)
	}
}

// Dropped answers a send that will never run because of a failure: pushed
// out of a full queue, discarded by a panic, or queued on a key the router
// retired. A reset or a shutdown is already visible to the tab (reset ack,
// closing socket).
func (o *wsOrigin) Dropped(_ context.Context, why turn.DropReason) {
	switch why {
	case turn.DropEvicted:
		o.fail(evictedSendMsg)
	case turn.DropPanic:
		o.fail(turnPanicMsg)
	case turn.DropRemoved:
		o.fail(removedSendMsg)
	}
}

func (o *wsOrigin) fail(msg string) {
	o.c.SendJSON(wsproto.NewSendAck(wsproto.SendAck{ID: o.id, Status: "error", Key: o.key, Error: msg}))
}

// httpOrigin is one HTTP send. Its receiver speaks for every HTTP send on the
// key in the batch (the Mates), so a failed turn is broadcast once. The
// broadcast reaches every tab on the key, so informational outcomes are
// dropped: B's /new resetting A's send must not tear down B's own bubble.
type httpOrigin struct {
	dashOrigin
	notify sendNotifier
}

func (o *httpOrigin) Sink() string { return "http:" + o.key }

func (o *httpOrigin) Begin(_ context.Context, t turn.TurnInfo) turn.Delivery {
	if t.Role == turn.RoleObserver {
		return nil
	}
	return o
}

func (o *httpOrigin) Finish(_ context.Context, out turn.Outcome) {
	if msg, failed, err := o.failure(out); failed && !informationalSendErr(err) {
		o.notify.broadcastSendError(o.key, msg)
	}
}

// Dropped says nothing: HTTP has no per-request channel after its 202.
func (*httpOrigin) Dropped(context.Context, turn.DropReason) {}

// wsOrigin and httpOrigin build the origins the two transports pass to
// sessionSend.
func (e *sendEngine) wsOrigin(c *wsClient, id, key string) turn.Origin {
	return &wsOrigin{dashOrigin: dashOrigin{key: key, opts: e.sessionOptsFor}, c: c, id: id}
}

func (e *sendEngine) httpOrigin(key string) turn.Origin {
	return &httpOrigin{dashOrigin: dashOrigin{key: key, opts: e.sessionOptsFor}, notify: e.notify}
}

// dashAdmission runs every dashboard turn, owner loop or detached, on its
// own goroutine registered with the engine's TrackSend, on the engine's
// ctx; it declines once drain has started. release is deferred so the slot
// is freed however the turn ends.
type dashAdmission struct {
	track func() (release func(), shuttingDown bool)
	ctx   context.Context
}

func (a dashAdmission) Admit(turn.RunKind) (func(fn func(ctx context.Context)), bool) {
	release, shuttingDown := a.track()
	if shuttingDown {
		return nil, false
	}
	return func(fn func(ctx context.Context)) {
		go func() {
			defer release()
			fn(a.ctx)
		}()
	}, true
}
