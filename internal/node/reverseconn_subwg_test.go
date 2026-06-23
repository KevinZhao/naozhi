package node

import (
	"sync"
	"testing"
	"time"
)

// holdSink records whether SendJSON is ever called after Close() returns.
// The Subscribe history goroutine calls SendJSON; if Close() does not wait on
// subWG, that write can race the teardown and land after Close returns.
type holdSink struct {
	mu          sync.Mutex
	closedAt    time.Time // set by the test right after rc.Close() returns
	lateSends   int       // SendJSON calls observed after closedAt was set
	enter       chan struct{}
	releaseHold chan struct{}
}

func (s *holdSink) SendJSON(v any) {
	// Signal entry, then block until released so the test can call Close()
	// while this goroutine is mid-flight.
	if s.enter != nil {
		select {
		case s.enter <- struct{}{}:
		default:
		}
	}
	if s.releaseHold != nil {
		<-s.releaseHold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closedAt.IsZero() {
		s.lateSends++
	}
}

func (s *holdSink) SendRaw(data []byte) {}

// TestReverseConn_CloseWaitsForSubscribeHistoryGoroutine pins R202606f-GO-010
// (#2294): Close() must not return while a Subscribe history-fetch goroutine
// is still mid-SendJSON. We drive the additional-subscriber path (alreadySub),
// whose goroutine sends "subscribed" then "history". By blocking the sink
// inside SendJSON we hold the goroutine alive across Close(); a correct
// implementation (subWG.Wait()) blocks Close() until we release the sink.
func TestReverseConn_CloseWaitsForSubscribeHistoryGoroutine(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	// Echo any RPC with a valid (empty) events response so FetchEvents returns
	// quickly and the goroutine proceeds to SendJSON.
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			var msg ReverseMsg
			if err := wsConn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type == "request" && msg.Method == "fetch_events" {
				_ = wsConn.WriteJSON(ReverseMsg{
					Type:   "response",
					ReqID:  msg.ReqID,
					Result: []byte(`[]`),
				})
			}
		}
	}()

	// Pre-seed a subscriber so the next Subscribe takes the alreadySub path
	// (which always reaches SendJSON, unlike the first-subscriber path that
	// only sends when there are persisted events).
	seed := &mockSink{id: 99}
	rc.subMu.Lock()
	rc.subs["k"] = append(rc.subs["k"], seed)
	rc.subMu.Unlock()

	sink := &holdSink{
		enter:       make(chan struct{}, 1),
		releaseHold: make(chan struct{}),
	}
	rc.Subscribe(sink, "k", 0)

	// Wait until the goroutine is parked inside SendJSON.
	select {
	case <-sink.enter:
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe history goroutine never reached SendJSON")
	}

	closeReturned := make(chan struct{})
	go func() {
		rc.Close()
		close(closeReturned)
	}()

	// Close() must block while the goroutine is held in SendJSON.
	select {
	case <-closeReturned:
		t.Fatal("Close() returned while Subscribe history goroutine still in SendJSON (subWG not waited)")
	case <-time.After(200 * time.Millisecond):
		// Good: Close is blocked on subWG.Wait().
	}

	// Mark the close boundary and release the goroutine.
	sink.mu.Lock()
	sink.closedAt = time.Now()
	sink.mu.Unlock()
	close(sink.releaseHold)

	select {
	case <-closeReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return after goroutine released")
	}
}

// TestReverseConn_CloseNoSubscribersReturnsPromptly guards the common case:
// with no Subscribe goroutines outstanding, Close() must not block on
// subWG.Wait().
func TestReverseConn_CloseNoSubscribersReturnsPromptly(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()

	done := make(chan struct{})
	go func() {
		rc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() blocked with no outstanding Subscribe goroutines")
	}
}
