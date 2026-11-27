package server

// wshub_eventpush_same_ms_test.go — #2402 regression: the dashboard event
// pusher must deliver an entry that lands in the SAME wall-clock millisecond
// as the tail of an already-delivered wave but arrives in a LATER notify
// wave.
//
// This is the local-dashboard twin of R20260530-GO-1 (#1481, fixed in
// internal/upstream). The ACP (kiro) backend hits it on EVERY turn:
// ACPProtocol.ReadEvent synthesises the turn-end (thinking, text, result)
// events from one stdout frame and each is appended via its own AppendBatch
// call within the same millisecond, with a subscriber notify in between. The
// old eventPushLoop tracked a naked lastTime int64 and queried
// EventEntriesSince(lastTime) (strictly greater-than): a pushLoop that woke
// between the appends advanced lastTime to T after seeing only the first
// entry, and the remaining same-millisecond entries — including the visible
// reply text — never matched `Time > T` again. Operator-visible as "kiro
// output doesn't show until you switch away and back" (the re-subscribe's
// full history fetch bypasses the cursor).

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// readUntilHistoryWith reads server messages until a "history" frame carrying
// an entry with the given UUID arrives, or the deadline passes. Returns the
// matched entry's summary and true on success. Non-history frames
// (session_state etc.) are skipped.
func readUntilHistoryWith(t *testing.T, conn interface {
	ReadJSON(v any) error
}, uuid string, deadline time.Time) (string, bool) {
	t.Helper()
	for time.Now().Before(deadline) {
		var resp node.ServerMsg
		if err := conn.ReadJSON(&resp); err != nil {
			return "", false
		}
		if resp.Type != "history" {
			continue
		}
		for _, e := range resp.Events {
			if e.UUID == uuid {
				return e.Summary, true
			}
		}
	}
	return "", false
}

// TestEventPush_SameMillisecondNoDuplicates guards the dedup half of the
// cursor: after the same-millisecond redelivery window, already-sent UUIDs
// must not be pushed again on subsequent waves.
func TestEventPush_SameMillisecondNoDuplicates(t *testing.T) {
	hub, router := newTestHub(t, "")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	if resp := wsRead(t, conn); resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}

	const ms = 7000
	proc.EventLog.Append(clievent.EventEntry{Time: ms, UUID: "a", Type: "text", Summary: "a"})
	deadline := time.Now().Add(3 * time.Second)
	if _, ok := readUntilHistoryWith(t, conn, "a", deadline); !ok {
		t.Fatal("entry a never delivered")
	}
	proc.EventLog.Append(clievent.EventEntry{Time: ms, UUID: "b", Type: "text", Summary: "b"})
	// Follow-up wave at a LATER millisecond: c must arrive, and the frames
	// observed along the way must not contain a second copy of "a".
	proc.EventLog.Append(clievent.EventEntry{Time: ms + 10, UUID: "c", Type: "text", Summary: "c"})

	seen := map[string]int{"a": 1} // a was already delivered once above
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var resp node.ServerMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("ws read: %v (seen=%v)", err, seen)
		}
		if resp.Type != "history" {
			continue
		}
		for _, e := range resp.Events {
			seen[e.UUID]++
		}
		if seen["b"] > 0 && seen["c"] > 0 {
			break
		}
	}
	if seen["b"] == 0 || seen["c"] == 0 {
		t.Fatalf("missing entries: seen=%v, want b and c delivered", seen)
	}
	if seen["a"] > 1 {
		t.Fatalf("entry a delivered %d times, want exactly 1 — the cursor's "+
			"UUID dedup at the watermark millisecond failed", seen["a"])
	}
}
