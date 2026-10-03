package server

// wshub_eventpush_burst_test.go — #3008: a notify wave larger than one
// history frame must reach the subscriber in full and in order, the cursor
// never advances past an entry that was not delivered, and a dropped frame is
// retried without waiting for another Append.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const (
	burstLen     = 120
	burstBaseMS  = 10_000
	burstSharedA = 48 // entries [burstSharedA, burstSharedB] share one Time,
	burstSharedB = 52 // straddling the first chunk boundary (index 50)
)

// burstEntries builds burstLen chronological entries with explicit UUIDs.
func burstEntries() []clievent.EventEntry {
	out := make([]clievent.EventEntry, burstLen)
	for i := range out {
		ts := int64(burstBaseMS + i)
		if i >= burstSharedA && i <= burstSharedB {
			ts = burstBaseMS + burstSharedA
		}
		out[i] = clievent.EventEntry{Time: ts, UUID: fmt.Sprintf("burst-%03d", i), Type: "text", Summary: "x"}
	}
	return out
}

// assertBurstInOrder fails unless got is exactly burstEntries' UUIDs in order.
func assertBurstInOrder(t *testing.T, got []string) {
	t.Helper()
	if len(got) != burstLen {
		t.Fatalf("delivered %d entries, want all %d: %v", len(got), burstLen, got)
	}
	for i, u := range got {
		if want := fmt.Sprintf("burst-%03d", i); u != want {
			t.Fatalf("entry %d = %q, want %q (out of order, duplicated or missing)", i, u, want)
		}
	}
}

func TestEventPush_BurstLargerThanOneFrame_DeliversEveryEntryInOrder(t *testing.T) {
	hub, router := newTestHub(t, "")
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "seed", Type: "user", Summary: "hi"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()
	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	if resp := wsRead(t, conn); resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}
	if resp := wsRead(t, conn); resp.Type != "history" || len(resp.Events) != 1 {
		t.Fatalf("initial history = %+v, want 1 seed entry", resp)
	}

	// One AppendBatch = one notify wave holding all 120 entries.
	proc.EventLog.AppendBatch(burstEntries())

	var got []string
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < burstLen && time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var resp node.ServerMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("ws read after %d entries: %v", len(got), err)
		}
		if resp.Type != "history" {
			continue
		}
		if len(resp.Events) > maxHistoryPushEntries {
			t.Fatalf("history frame carries %d entries, want <= %d", len(resp.Events), maxHistoryPushEntries)
		}
		for _, e := range resp.Events {
			got = append(got, e.UUID)
		}
	}
	assertBurstInOrder(t, got)
}

// A frame dropped on a full send buffer must not advance the cursor: the
// next wave resumes at the dropped chunk, so every entry still arrives once.
func TestBackfillSubscriberEvents_DroppedFrameDoesNotAdvanceCursor(t *testing.T) {
	hub, router := newTestHub(t, "")
	proc := session.NewTestProcess()
	const key = "test:d:u:general"
	sess := router.InjectSession(key, proc)
	burst := burstEntries()
	proc.EventLog.AppendBatch(burst)

	c := &wsClient{send: make(chan []byte, 1), done: make(chan struct{})}
	csr := clievent.NewSinceCursor()
	var buf []clievent.EventEntry
	var got []string
	// Each wave fits one frame into the cap-1 buffer and drops the next.
	wantWM := []int64{burst[49].Time, burst[99].Time, burst[burstLen-1].Time}
	wantDropped := []int64{1, 2, 2}
	wantPending := []bool{true, true, false}
	for wave := range wantWM {
		alive, pending, b := hub.backfillSubscriberEvents(c, key, sess, csr, buf)
		buf = b
		if !alive {
			t.Fatalf("wave %d: alive = false on an open client", wave)
		}
		if pending != wantPending[wave] {
			t.Fatalf("wave %d: pending = %v, want %v", wave, pending, wantPending[wave])
		}
		if wm := csr.Watermark(); wm != wantWM[wave] {
			t.Fatalf("wave %d: watermark = %d, want %d (cursor advanced past an unsent frame)", wave, wm, wantWM[wave])
		}
		if d := c.dropped.Load(); d != wantDropped[wave] {
			t.Fatalf("wave %d: dropped = %d, want %d", wave, d, wantDropped[wave])
		}
		if len(c.send) != 1 {
			t.Fatalf("wave %d: %d frames queued, want 1", wave, len(c.send))
		}
		var msg node.ServerMsg
		if err := json.Unmarshal(<-c.send, &msg); err != nil {
			t.Fatalf("wave %d: decode frame: %v", wave, err)
		}
		if len(msg.Events) > maxHistoryPushEntries {
			t.Fatalf("wave %d: frame carries %d entries, want <= %d", wave, len(msg.Events), maxHistoryPushEntries)
		}
		for _, e := range msg.Events {
			got = append(got, e.UUID)
		}
	}
	assertBurstInOrder(t, got)

	// Fully caught up: a further wave sends nothing.
	if alive, pending, _ := hub.backfillSubscriberEvents(c, key, sess, csr, buf); !alive || pending || len(c.send) != 0 {
		t.Fatalf("caught-up wave: alive = %v, pending = %v, queued = %d; want true, false, 0", alive, pending, len(c.send))
	}
}

// A dropped frame in an idle session (no further Append, so no notify) is
// retried on the push loop's timer until the whole wave has arrived.
func TestEventPushLoop_DroppedFrameRetriedWithoutNextAppend(t *testing.T) {
	hub, router := newTestHub(t, "")
	defer hub.Shutdown()
	hub.historyRetryInterval = 5 * time.Millisecond
	proc := session.NewTestProcess()
	const key = "test:d:u:general"
	sess := router.InjectSession(key, proc)

	c := newTestWSClient()
	c.send = make(chan []byte, 1)
	defer c.closeDone()
	hub.register(c)
	notify, unsub := sess.SubscribeEvents()
	gen := subscribeTest(hub, c, key, unsub)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		hub.eventPushLoop(c, key, gen, notify, sess, clievent.NewSinceCursor())
	}()

	// The only notify of the test: the loop fills the cap-1 buffer and drops
	// the next frame before anything is read.
	proc.EventLog.AppendBatch(burstEntries())
	testhelper.Eventually(t, func() bool { return c.dropped.Load() > 0 }, 3*time.Second,
		"no frame dropped on the cap-1 send buffer")

	deadline := time.Now().Add(3 * time.Second)

	var got []string
	for len(got) < burstLen {
		select {
		case data := <-c.send:
			var msg node.ServerMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			for _, e := range msg.Events {
				got = append(got, e.UUID)
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("delivered %d of %d entries; the dropped frame was never retried", len(got), burstLen)
		}
	}
	assertBurstInOrder(t, got)

	c.closeDone()
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("eventPushLoop did not exit after the client closed")
	}
}
