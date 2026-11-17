package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestPassthrough_PeerReplayClaimsNoSlot: a cross-session message the CLI runs
// as its own turn arrives as an isReplay user frame with a CLI uuid. It must
// not sweep a queued Send's slot, or that Send returns the peer turn's answer.
func TestPassthrough_PeerReplayClaimsNoSlot(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	type sendOut struct {
		res *clievent.SendResult
		err error
	}
	out := make(chan sendOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "hello", nil, nil, "")
		out <- sendOut{res, err}
	}()
	input := sh.expectWrite(t, 2*time.Second)

	// The peer message was dequeued first and runs alone (fixture shape from
	// peer_frames_test.go).
	sh.emitInit("s1")
	sh.srv.SendStdout(`{"type":"user","message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/2.sock\" from-name=\"planner\" from-mode=\"dontAsk\">\n在忙吗\n</cross-session-message>"},"uuid":"8bd2fc7b-a013-49d9-8152-22b5a1bdcc9b","isReplay":true,"isSynthetic":true,"origin":{"kind":"peer","name":"planner","fromMode":"dontAsk","body":"在忙吗"}}`)
	sh.emitResult("s1", "PEER ANSWER")

	sh.emitInit("s1")
	sh.emitReplay(input.UUID, "hello")
	sh.emitResult("s1", "A ANSWER")

	select {
	case o := <-out:
		if o.err != nil {
			t.Fatalf("SendPassthrough: %v", o.err)
		}
		if o.res.Text != "A ANSWER" {
			t.Errorf("result.Text = %q, want %q", o.res.Text, "A ANSWER")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendPassthrough did not return within 3s")
	}
}
