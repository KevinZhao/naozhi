package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/turn"
)

// postSendJSON issues an authenticated JSON POST to handleSend and returns the
// recorder. Bearer auth keeps the upload owner deterministic (bearerOwner).
func postSendJSON(t *testing.T, h *SendHandler, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/send", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.handleSend(w, r)
	return w
}

// TestHandleSend_AsyncFailureReachesSubscribers pins the HTTP half of the
// #2418 follow-up (F1): file-bearing sends now always travel over HTTP, whose
// 202 ack is the last thing the client hears. When the owner goroutine later
// fails (spawn error / passthrough send failure) the failure must still reach
// the dashboard — as a `send_error` frame to every subscriber of the key —
// instead of being dropped by a nil onAsyncError callback.
//
// The test hub has no CLI wrapper wired, so GetOrCreate fails fast inside the
// owner goroutine, exercising exactly the asynchronous branch.
func TestHandleSend_AsyncFailureReachesSubscribers(t *testing.T) {
	const key = "test:d:u:general"
	hub, _ := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)

	watcher, watcherOut := newCapturedClient(t, hub)
	registerSub(hub, watcher, key)
	other, otherOut := newCapturedClient(t, hub)
	registerSub(hub, other, "test:d:u:elsewhere")

	h := &SendHandler{engine: hub.engine, uploadStore: newUploadStore()}
	w := postSendJSON(t, h, "tok", map[string]any{"key": key, "text": "hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s, want 202 (failure is asynchronous)", w.Code, w.Body.String())
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-watcherOut:
			if m.Type != "send_error" {
				continue // session_state / sessions_update noise
			}
			if m.Key != key {
				t.Fatalf("send_error Key = %q, want %q", m.Key, key)
			}
			if m.Error == "" {
				t.Fatal("send_error must carry a user-facing error label")
			}
			// Must be the localised label, never the raw error (paths / keys).
			if strings.Contains(m.Error, "/") || strings.Contains(m.Error, key) {
				t.Fatalf("send_error leaks internal detail: %q", m.Error)
			}
			// Scoped to the key's subscribers only.
			select {
			case o := <-otherOut:
				if o.Type == "send_error" {
					t.Fatalf("non-subscriber received send_error: %+v", o)
				}
			case <-time.After(100 * time.Millisecond):
			}
			return
		case <-deadline:
			t.Fatal("subscriber never received send_error after async spawn failure")
		}
	}
}

// TestBroadcastSendError_Scoping mirrors the broadcastSessionSystemEvent
// contract for the new frame: subscribers of the key get it, everyone else
// stays silent, and empty args are a no-op.
func TestBroadcastSendError_Scoping(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: skipped in -short")
	}
	hub, _ := newTestHub(t, "tok")
	t.Cleanup(hub.Shutdown)

	sub, subOut := newCapturedClient(t, hub)
	registerSub(hub, sub, "feishu:p2p:alice")
	other, otherOut := newCapturedClient(t, hub)
	registerSub(hub, other, "feishu:p2p:bob")

	hub.bcast.broadcastSendError("", "x")
	hub.bcast.broadcastSendError("feishu:p2p:alice", "")
	if _, ok := recvMsg(t, subOut); ok {
		t.Fatal("empty key/error must emit nothing")
	}

	hub.bcast.broadcastSendError("feishu:p2p:alice", "会话启动失败")
	msg, ok := recvMsg(t, subOut)
	if !ok {
		t.Fatal("subscriber received no frame")
	}
	if msg.Type != "send_error" || msg.Key != "feishu:p2p:alice" || msg.Error != "会话启动失败" {
		t.Fatalf("unexpected frame: %+v", msg)
	}
	if _, ok := recvMsg(t, otherOut); ok {
		t.Fatal("non-subscriber must not receive send_error")
	}
}

// TestHTTPOrigin_SkipsInformationalErrors pins the M1 rule: the HTTP origin
// fans a failed turn out to EVERY subscriber of the key, so outcomes the user
// already knows about (ErrSessionReset / ErrReconnectedUnknown —
// session_state corrects the UI) must not become a
// send_error that would tear down a second tab's own optimistic state. The WS
// origin keeps reporting them because it can address the originator alone.
// Real failures (spawn error) and a panic must still fan out.
func TestHTTPOrigin_SkipsInformationalErrors(t *testing.T) {
	const key = "feishu:p2p:alice"
	hub, _ := newTestHub(t, "tok")
	t.Cleanup(hub.Shutdown)
	sub, subOut := newCapturedClient(t, hub)
	registerSub(hub, sub, key)

	d := hub.engine.httpOrigin(key).Begin(context.Background(), turn.TurnInfo{Role: turn.RoleHead})
	for _, e := range []error{
		fmt.Errorf("passthrough: %w", clierr.ErrSessionReset), // wrapped — errors.Is, not ==
		clierr.ErrReconnectedUnknown,
	} {
		d.Finish(context.Background(), turn.Outcome{Stage: turn.StageSend, Err: e})
	}
	if m, ok := recvMsg(t, subOut); ok {
		t.Fatalf("informational error must not fan out, got %+v", m)
	}

	spawnErr := errors.New("spawn process: exec: no wrapper")
	d.Finish(context.Background(), turn.Outcome{Stage: turn.StageSession, Err: spawnErr})
	m, ok := recvMsg(t, subOut)
	if !ok || m.Type != "send_error" || m.Key != key || m.Error != asyncErrorMessage(spawnErr) {
		t.Fatalf("real failure must fan out as send_error, got ok=%v %+v", ok, m)
	}
	// A panicked turn has no error and must still reach subscribers.
	d.Finish(context.Background(), turn.Outcome{Stage: turn.StageSend, Panic: true})
	if m, ok := recvMsg(t, subOut); !ok || m.Error != turnPanicMsg {
		t.Fatalf("panic must fan out, got ok=%v %+v", ok, m)
	}
	d.Finish(context.Background(), turn.Outcome{Stage: turn.StageDone})
	if m, ok := recvMsg(t, subOut); ok {
		t.Fatalf("a successful turn must not fan out, got %+v", m)
	}
}

// TestWSOrigin_ReportsInformationalErrors is the WS side of the rule above:
// the error ack reaches the sending tab alone, so an informational outcome
// still clears that send's bubble.
func TestWSOrigin_ReportsInformationalErrors(t *testing.T) {
	const key = "feishu:p2p:alice"
	hub, _ := newTestHub(t, "tok")
	t.Cleanup(hub.Shutdown)
	c, out := newCapturedClient(t, hub)

	d := hub.engine.wsOrigin(c, "w1", key).Begin(context.Background(), turn.TurnInfo{Role: turn.RoleHead})
	for _, e := range []error{
		fmt.Errorf("passthrough: %w", clierr.ErrSessionReset),
		clierr.ErrReconnectedUnknown,
	} {
		d.Finish(context.Background(), turn.Outcome{Stage: turn.StageSend, Err: e})
		m, ok := recvMsg(t, out)
		if !ok || m.Type != "send_ack" || m.ID != "w1" || m.Status != "error" || m.Error != asyncErrorMessage(e) {
			t.Fatalf("%v: got ok=%v %+v, want an error ack for w1", e, ok, m)
		}
	}
}
