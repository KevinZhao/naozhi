package server

// wshub_event_contract_2432_test.go — #2432 backend→frontend event contract
// regressions:
//
//  1. historyMarshalCache fingerprint must include entry identity (UUID), not
//     just (watermark, latestTime, count). With ≥2 subscribers the ACP
//     turn-end shape (three same-millisecond entries arriving in three notify
//     waves) produced identical fingerprints for wave 2 `[e2(T)]` and wave 3
//     `[e3(T)]`, so wave 3 returned e2's cached bytes and e3 was never sent.
//  2. Reconnect `subscribe{after}` must re-admit the `after` millisecond so a
//     later same-ms sibling of the last delivered entry is not lost forever;
//     the dashboard dedups same-ms replays by UUID (onHistory / cron-live).
//  3. Every subscribe gets exactly one Initial history frame — including a
//     zero-event session with no process or a non-running process — so the
//     dashboard can render its "暂无事件" placeholder instead of a blank pane.

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestEventPush_MultiSubscriber_SameMsWaves_DistinctUUIDs (#2432 P2).
func TestEventPush_MultiSubscriber_SameMsWaves_DistinctUUIDs(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "seed", Type: "user", Summary: "hi"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conns := []interface {
		ReadJSON(v any) error
	}{}
	for i := 0; i < 2; i++ {
		conn := dialWS(t, url)
		defer conn.Close()
		wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
		if resp := wsRead(t, conn); resp.Type != "subscribed" {
			t.Fatalf("conn %d: type = %q, want subscribed", i, resp.Type)
		}
		if resp := wsRead(t, conn); resp.Type != "history" || len(resp.Events) != 1 {
			t.Fatalf("conn %d: initial history = %+v, want 1 seed entry", i, resp)
		}
		conns = append(conns, conn)
	}
	if hub.singleSubscriber("test:d:u:general") {
		t.Fatal("test setup: expected 2 subscribers so the marshal cache path is exercised")
	}

	// ACP turn-end shape: three entries, one millisecond, three notify waves.
	// Wait for every subscriber to receive each wave before appending the
	// next so each pushLoop's cursor is at T with exactly one entry per wave.
	const turnEndMS = 5000
	for _, uuid := range []string{"e1", "e2", "e3"} {
		proc.EventLog.Append(clievent.EventEntry{Time: turnEndMS, UUID: uuid, Type: "text", Summary: uuid})
		for i, conn := range conns {
			deadline := time.Now().Add(3 * time.Second)
			if _, ok := readUntilHistoryWith(t, conn, uuid, deadline); !ok {
				t.Fatalf("#2432 regression: subscriber %d never received %s — "+
					"historyMarshalCache fingerprint (watermark, latestTime, count) "+
					"collided across same-millisecond waves and served stale bytes", i, uuid)
			}
		}
	}
}

// TestWS_SubscribeWithAfter_ReadmitsAfterMillisecond (#2432 P3): a reconnect
// with after=T must return the entries AT T as well (frontend dedups by UUID)
// so a same-ms sibling appended after the client's last delivery is not lost.
func TestWS_SubscribeWithAfter_ReadmitsAfterMillisecond(t *testing.T) {
	hub, router := newTestHub("")
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"})
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "a", Type: "thinking", Summary: "..."})
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "b", Type: "text", Summary: "reply"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// Client saw "a" (lastEventTimeWs=2000), dropped, and reconnects with after=2000.
	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general", After: 2000})
	if resp := wsRead(t, conn); resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}
	resp := wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("type = %q, want history", resp.Type)
	}
	got := map[string]bool{}
	for _, e := range resp.Events {
		got[e.UUID] = true
	}
	if got["old"] {
		t.Fatalf("after=2000 must exclude time=1000 entry, got %+v", resp.Events)
	}
	if !got["a"] || !got["b"] {
		t.Fatalf("#2432 regression: after=2000 returned %+v, want both same-ms entries a,b "+
			"(strict > drops the sibling appended after the client's last delivery forever)", resp.Events)
	}
}

// TestWS_Subscribe_ZeroEvents_AlwaysSendsInitialHistory (#2432 P3): a ready
// (non-running) process with an empty log and a detached (no-process) session
// must both answer the initial subscribe with an empty Initial history frame.
func TestWS_Subscribe_ZeroEvents_AlwaysSendsInitialHistory(t *testing.T) {
	cases := []struct {
		name string
		proc *session.TestProcess
	}{
		{"ready process, empty log", session.NewTestProcess()},
		{"no process (stub)", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, router := newTestHub("")
			router.InjectSession("test:d:u:general", tc.proc)

			url, cleanup := startWSServer(t, hub)
			defer cleanup()

			conn := dialWS(t, url)
			defer conn.Close()

			wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general", Limit: 50})
			if resp := wsRead(t, conn); resp.Type != "subscribed" {
				t.Fatalf("type = %q, want subscribed", resp.Type)
			}
			resp := wsRead(t, conn)
			if resp.Type != "history" {
				t.Fatalf("#2432 regression: type = %q, want an (empty) Initial history frame so the "+
					"dashboard renders its placeholder instead of a blank pane", resp.Type)
			}
			if !resp.Initial {
				t.Fatalf("history frame Initial = false, want true")
			}
			if len(resp.Events) != 0 {
				t.Fatalf("events = %d, want 0", len(resp.Events))
			}
		})
	}
}
