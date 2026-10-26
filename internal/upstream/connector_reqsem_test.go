package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestReqSem_CapsAndReleasesSlots: reqSem admits 16 concurrent reverse-RPC
// requests, parks the 17th until a slot frees, and is idle again once every
// request has retired — a second wave of 16 is admitted at once.
func TestReqSem_CapsAndReleasesSlots(t *testing.T) {
	const capacity = 16
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// Deferred, not t.Cleanup: the fake server's Close (a Cleanup) waits for
	// the handler, which waits on these channels.
	defer releaseAll()

	var parked atomic.Int32
	secondWave := make(chan struct{})
	var waveOnce sync.Once
	startSecondWave := func() { waveOnce.Do(func() { close(secondWave) }) }
	defer startSecondWave()
	responses := make(chan struct{}, 2*capacity+1)

	send := func(conn *websocket.Conn, prefix string, n int) bool {
		for i := 0; i < n; i++ {
			params, _ := json.Marshal(map[string]string{})
			req := node.ReverseMsg{Type: "request", ReqID: fmt.Sprintf("%s-%d", prefix, i), Method: "fetch_discovered_preview", Params: params}
			if err := conn.WriteJSON(req); err != nil {
				return false
			}
		}
		return true
	}
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		// Empty session_id short-circuits the handler to the preview func.
		if !send(conn, "a", capacity+1) {
			return
		}
		go func() {
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			for {
				var resp node.ReverseMsg
				if err := conn.ReadJSON(&resp); err != nil {
					return
				}
				responses <- struct{}{}
			}
		}()
		<-secondWave
		send(conn, "b", capacity)
		<-release
	})

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, testRouter(makeRouter()), nil, nil, Discovery{}, nil)
	var calls atomic.Int32
	gate := make(chan struct{})
	c.discovery.Preview = func(string) (json.RawMessage, error) {
		parked.Add(1)
		defer parked.Add(-1)
		if calls.Add(1) <= capacity+1 {
			<-gate // first wave
		} else {
			<-release
		}
		return json.RawMessage("[]"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	testhelper.Eventually(t, func() bool { return parked.Load() == capacity }, 5*time.Second, "first wave never filled the semaphore")
	time.Sleep(50 * time.Millisecond)
	if n := parked.Load(); n != capacity {
		t.Fatalf("%d requests in flight, want the cap %d", n, capacity)
	}

	close(gate)
	testhelper.Eventually(t, func() bool { return len(responses) == capacity+1 }, 5*time.Second, "first wave did not all answer")
	testhelper.Eventually(t, func() bool { return parked.Load() == 0 }, 5*time.Second, "slots still held after the first wave")

	startSecondWave()
	testhelper.Eventually(t, func() bool { return parked.Load() == capacity }, 5*time.Second, "semaphore not idle: second wave was not admitted")

	releaseAll()
	cancel()
	<-done
}
