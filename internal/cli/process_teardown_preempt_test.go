package cli

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestProcess_TeardownPreemptsPinnedWriter: a plain sender (shimSend, the
// stdin path) sets no write deadline, so against a shim that has stopped
// reading it blocks while holding shimWMu. Every teardown path takes that
// lock, so each must first make the pinned write fail. Otherwise Close, Kill
// and Detach wait for as long as the shim is wedged.
//
// The order is fixed rather than raced: the sender is seen holding shimWMu
// (it is the only contender) before the teardown starts.
func TestProcess_TeardownPreemptsPinnedWriter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		teardown func(p *Process)
	}{
		{"Close", (*Process).Close},
		{"Kill", (*Process).Kill},
		{"Detach", (*Process).Detach},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, srv := shimTestPair(&ClaudeProtocol{})
			t.Cleanup(func() { srv.conn.Close() })

			sent := make(chan error, 1)
			go func() { sent <- p.shimSend(shimClientMsg{Type: "ping"}) }()
			testhelper.Eventually(t, func() bool {
				if p.shimWMu.TryLock() {
					p.shimWMu.Unlock()
					return false
				}
				return true
			}, 2*time.Second, "the sender never took shimWMu")

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.teardown(p)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not return while a sender held shimWMu on a shim that never reads", tc.name)
			}
			// The sender releases shimWMu before it reports, so its result can
			// trail the teardown's return slightly; wait for it, boundedly.
			select {
			case err := <-sent:
				if err == nil {
					t.Error("the pinned write reported success; it can only have been cut short")
				}
			case <-time.After(5 * time.Second):
				t.Error("the pinned sender was still blocked after teardown returned")
			}
		})
	}
}
