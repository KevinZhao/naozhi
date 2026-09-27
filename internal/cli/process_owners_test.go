package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestControlAcks_DeliverReachesTheWaiterOnce: an ack reaches the waiter that
// registered its request_id, once; the CLI's rejection arrives as
// ErrSetModelRejected; an ack for an unregistered or already-answered id is
// dropped.
func TestControlAcks_DeliverReachesTheWaiterOnce(t *testing.T) {
	p := &Process{}
	ok := p.acks.register("req-ok")
	rejected := p.acks.register("req-bad")

	p.deliverControlAck(clievent.Event{Type: "control_ack", RPCRequestID: "req-ok"})
	p.deliverControlAck(clievent.Event{Type: "control_ack", RPCRequestID: "req-bad", SubType: "error", Result: "no such model"})
	if err := waitAck(t, ok); err != nil {
		t.Errorf("ack for req-ok = %v, want nil", err)
	}
	if err := waitAck(t, rejected); !errors.Is(err, clierr.ErrSetModelRejected) {
		t.Errorf("ack for req-bad = %v, want ErrSetModelRejected", err)
	}

	// Answered ids are gone: a duplicate ack finds no waiter (and must not
	// block on the full channel).
	p.deliverControlAck(clievent.Event{Type: "control_ack", RPCRequestID: "req-ok"})
	if _, still := p.acks.take("req-ok"); still {
		t.Error("an answered request_id still has a waiter")
	}
}

// TestControlAcks_UnregisterDropsTheWaiter: a waiter that gave up (timed
// out) leaves no entry behind for a late ack.
func TestControlAcks_UnregisterDropsTheWaiter(t *testing.T) {
	var a controlAcks
	a.register("req-late")
	a.unregister("req-late")
	if _, still := a.take("req-late"); still {
		t.Error("an unregistered request_id still has a waiter")
	}
}

func waitAck(t *testing.T, ch chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the ack never reached its waiter")
		return nil
	}
}
