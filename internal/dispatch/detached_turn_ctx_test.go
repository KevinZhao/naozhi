package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestDetachedTurn_CtxOutlivesInboundAndFollowsStop pins imAdmission's ctx
// for a detached turn at the entry points (#1320): the Send ctx survives the
// webhook handler's ctx being cancelled, keeps its values, and is cancelled
// by the dispatcher's StopCtx. The mergeStopAndValues helper has its own
// tests; this one fails if Admit hands the turn the inbound ctx instead.
func TestDetachedTurn_CtxOutlivesInboundAndFollowsStop(t *testing.T) {
	type inboundKey struct{}
	cases := []struct {
		name string
		mode turn.Mode
		text string
	}{
		{"passthrough", turn.ModePassthrough, "hi"},
		{"urgent", turn.ModeCollect, "/urgent hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stop, cancelStop := context.WithCancel(context.Background())
			defer cancelStop()
			sendCtx := make(chan context.Context, 1)
			release := make(chan struct{})
			defer close(release)
			sender := &testSender{
				getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
					return fakeSession{}, session.SessionExisting, nil
				},
				send: func(ctx context.Context, _ string, _ turn.Session, _ string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
					sendCtx <- ctx
					<-release
					return &clievent.SendResult{Text: "ok"}, nil
				},
			}
			d := newTestDispatcher(&fakePlatform{},
				withQueue(turn.NewQueueWithMode(5, 0, tc.mode)), withSender(sender),
				func(cfg *testDispatcherConfig) { cfg.StopCtx = stop })

			inbound, cancelInbound := context.WithCancel(context.WithValue(context.Background(), inboundKey{}, "req"))
			d.BuildHandler()(inbound, incomingMsg(tc.text))
			// The webhook handler returns; its ctx goes with it.
			cancelInbound()

			var ctx context.Context
			select {
			case ctx = <-sendCtx:
			case <-time.After(5 * time.Second):
				t.Fatal("detached turn never reached Send")
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("Send ctx cancelled with the inbound ctx: %v", err)
			}
			if got := ctx.Value(inboundKey{}); got != "req" {
				t.Errorf("Send ctx lost the inbound value: got %v", got)
			}
			cancelStop()
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("Send ctx not cancelled by StopCtx")
			}
		})
	}
}
