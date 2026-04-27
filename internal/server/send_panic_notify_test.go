package server

// RETRY3 regression tests for the dashboard (Hub) ownerLoop panic path.
// Hub.handleOwnerLoopPanic mirrors dispatch.handleOwnerLoopPanic: it logs,
// Discards the queue, and calls onAsyncError so the WS client sees "处理
// 异常，请刷新页面或稍后重试。" instead of silently losing its message.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
)

func TestHandleOwnerLoopPanic_CallsOnAsyncError(t *testing.T) {
	hub, _ := newTestHub("")
	hub.queue = dispatch.NewMessageQueue(5, 0)
	t.Cleanup(hub.Shutdown)

	var (
		mu      sync.Mutex
		gotMsgs []string
	)
	onAsyncError := func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		gotMsgs = append(gotMsgs, msg)
	}

	hub.handleOwnerLoopPanic("key-a", onAsyncError, "synthetic test panic")

	mu.Lock()
	defer mu.Unlock()
	if len(gotMsgs) != 1 {
		t.Fatalf("onAsyncError call count = %d, want 1", len(gotMsgs))
	}
	if !strings.Contains(gotMsgs[0], "处理异常") {
		t.Errorf("onAsyncError message = %q, want contains %q", gotMsgs[0], "处理异常")
	}
}

func TestHandleOwnerLoopPanic_NilOnAsyncErrorNoCrash(t *testing.T) {
	// HTTP path uses nil onAsyncError because the 202 ack has already
	// been shipped; the recover path must tolerate that silently.
	hub, _ := newTestHub("")
	hub.queue = dispatch.NewMessageQueue(5, 0)
	t.Cleanup(hub.Shutdown)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil onAsyncError path panicked: %v", r)
		}
	}()
	hub.handleOwnerLoopPanic("key-b", nil, "synthetic test panic")
}

func TestHandleOwnerLoopPanic_DiscardsQueue(t *testing.T) {
	hub, _ := newTestHub("")
	hub.queue = dispatch.NewMessageQueue(5, 0)
	t.Cleanup(hub.Shutdown)

	key := "key-c"
	hub.queue.Enqueue(key, dispatch.QueuedMsg{Text: "m1", EnqueueAt: time.Now()})
	hub.queue.Enqueue(key, dispatch.QueuedMsg{Text: "m2", EnqueueAt: time.Now()})
	if depth := hub.queue.Depth(key); depth == 0 {
		t.Fatalf("setup: expected nonzero depth, got %d", depth)
	}

	hub.handleOwnerLoopPanic(key, nil, "synthetic test panic")

	if depth := hub.queue.Depth(key); depth != 0 {
		t.Errorf("queue depth after panic recover = %d, want 0", depth)
	}
}

func TestHandleOwnerLoopPanic_OnAsyncErrorPanicAbsorbed(t *testing.T) {
	// A broken WS writer (or any user-supplied onAsyncError) might panic
	// when the process is under duress. The nested recover inside
	// handleOwnerLoopPanic must swallow it so the outer defer finishes.
	hub, _ := newTestHub("")
	hub.queue = dispatch.NewMessageQueue(5, 0)
	t.Cleanup(hub.Shutdown)

	called := false
	onAsyncError := func(_ string) {
		called = true
		panic("synthetic onAsyncError panic")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nested panic escaped handleOwnerLoopPanic: %v", r)
		}
	}()
	hub.handleOwnerLoopPanic("key-d", onAsyncError, "synthetic test panic")
	if !called {
		t.Errorf("onAsyncError was not invoked")
	}
}
