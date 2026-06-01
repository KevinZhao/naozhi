package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// ─── fakeWSClient ───────────────────────────────────────────────────────
//
// A tiny in-memory wsClient stand-in. The real wsClient.SendJSON writes to
// a websocket connection; here we capture every emitted node.ServerMsg so
// tests can assert the agent_event / agent_meta / agent_done sequencing
// without hoisting a full ws test harness.
//
// We reuse the real *wsClient type because tailerRegistry.subs maps it as
// a key and tests rely on map identity. The trick is to point its send
// channel at a capture goroutine — easier than faking the whole conn+mux
// surface.

func newCapturedClient(t *testing.T, hub *Hub) (*wsClient, <-chan node.ServerMsg) {
	t.Helper()
	c := &wsClient{
		hub:  hub,
		send: make(chan []byte, 64),
		done: make(chan struct{}),
	}
	c.authenticated.Store(true)
	out := make(chan node.ServerMsg, 64)
	go func() {
		for {
			select {
			case data, ok := <-c.send:
				if !ok {
					close(out)
					return
				}
				var msg node.ServerMsg
				if err := json.Unmarshal(data, &msg); err == nil {
					out <- msg
				}
			case <-c.done:
				close(out)
				return
			}
		}
	}()
	t.Cleanup(func() { close(c.done) })
	return c, out
}

// writeJSONL writes one jsonl line describing an assistant text event at the
// given path. Returns the absolute path.
func writeJSONL(t *testing.T, dir, text string, append bool) string {
	t.Helper()
	path := filepath.Join(dir, "agent-tailer-test.jsonl")
	flag := os.O_CREATE | os.O_WRONLY
	if append {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	return path
}

// ─── tests ──────────────────────────────────────────────────────────────

// TestTailer_RegistryEnsureIdempotent: ensureTailer called twice with the
// same (key, taskID) returns the same tailer. RFC §3.5.4: the silent tailer
// started by Linker.OnResolve and the one a later WS agent_subscribe would
// attach to must be the same instance.
func TestTailer_RegistryEnsureIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeJSONL(t, dir, "hi", false)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	t1, ok1 := r.ensureTailer("k", "t1", "toolu_A", path)
	t2, ok2 := r.ensureTailer("k", "t1", "toolu_A", path)
	if !ok1 || !ok2 || t1 != t2 {
		t.Errorf("ensureTailer not idempotent: ok1=%v ok2=%v same=%v", ok1, ok2, t1 == t2)
	}
}

// TestTailer_CapacityRejected: past the 50-tailer cap, ensureTailer returns
// (nil, false) so the WS handler emits agent_subscribe_rejected{reason:"capacity"}.
func TestTailer_CapacityRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeJSONL(t, dir, "hi", false)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	// Fill to the cap.
	for i := 0; i < agentTailerMax; i++ {
		taskID := "t" + itoaSmall(i)
		_, ok := r.ensureTailer("k", taskID, "toolu", path)
		if !ok {
			t.Fatalf("unexpected reject at slot %d", i)
		}
	}
	_, ok := r.ensureTailer("k", "overflow", "toolu", path)
	if ok {
		t.Errorf("expected reject past cap, got ok")
	}
}

// TestTailer_AttachReplaysBufferedEvents: when a client subscribes after the
// tailer has already polled fresh events, those events must be replayed (not
// lost) to the new subscriber. This is the "silent tailer keeps buffer for
// late joiners" contract.
func TestTailer_AttachReplaysBufferedEvents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeJSONL(t, dir, "first", false)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	tl, ok := r.ensureTailer("k", "t1", "toolu", path)
	if !ok {
		t.Fatal("ensureTailer failed")
	}

	// Force one poll iteration so "first" lands in the buffer before the
	// client attaches. pollOnce returns true on success; we drive it
	// synchronously to avoid racing the 200 ms ticker.
	tl.pollOnce()

	c, out := newCapturedClient(t, nil)
	if !r.attach(tailerKey{"k", "t1"}, c) {
		t.Fatal("attach returned false")
	}

	var saw bool
	for i := 0; i < 5; i++ {
		select {
		case msg := <-out:
			if msg.Type == "agent_event" && msg.Event != nil && msg.Event.Summary == "first" {
				saw = true
			}
		case <-time.After(300 * time.Millisecond):
		}
		if saw {
			break
		}
	}
	if !saw {
		t.Errorf("buffered event not replayed on attach")
	}
}

// TestTailer_AttachEmptyBufferNoReplay: attaching before any transcript line
// has landed (empty buffer) must succeed and emit no agent_event replay. Guards
// the R249-PERF-4 (#926) follow-up short-circuit that skips the buffer pool
// Get/Put when len(t.buffered)==0 — the subscriber still attaches, it just has
// nothing to replay yet (live events arrive via the next pollOnce fan-out).
func TestTailer_AttachEmptyBufferNoReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeJSONL(t, dir, "later", false)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	if _, ok := r.ensureTailer("k", "t1", "toolu", path); !ok {
		t.Fatal("ensureTailer failed")
	}

	// Attach WITHOUT driving pollOnce first, so t.buffered is empty.
	c, out := newCapturedClient(t, nil)
	if !r.attach(tailerKey{"k", "t1"}, c) {
		t.Fatal("attach returned false on empty buffer")
	}

	// No agent_event (and no agent_meta, since meta is zero) should arrive.
	select {
	case msg := <-out:
		t.Fatalf("unexpected frame on empty-buffer attach: type=%q", msg.Type)
	case <-time.After(150 * time.Millisecond):
		// expected: silence
	}
}

// TestTailer_CloseTaskFiresAgentDone: parent-stream task_done triggers
// closeTask, which must fan an agent_done frame to all remaining
// subscribers and evict the tailer from the registry.
func TestTailer_CloseTaskFiresAgentDone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeJSONL(t, dir, "x", false)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	_, _ = r.ensureTailer("k", "t1", "toolu", path)
	c, out := newCapturedClient(t, nil)
	if !r.attach(tailerKey{"k", "t1"}, c) {
		t.Fatal("attach failed")
	}
	// Drain any initial buffered events so we observe the lifecycle frames
	// cleanly.
	drainChan(out, 50*time.Millisecond)

	r.closeTask("k", "t1", "completed")

	var sawDone bool
	deadline := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case msg := <-out:
			if msg.Type == "agent_done" && msg.TaskID == "t1" && msg.Status == "completed" {
				sawDone = true
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	if !sawDone {
		t.Errorf("agent_done not received after closeTask")
	}

	// Tailer must be gone from the registry — a subsequent ensureTailer
	// call creates a fresh one.
	r.mu.RLock()
	_, stillThere := r.byTask[tailerKey{"k", "t1"}]
	r.mu.RUnlock()
	if stillThere {
		t.Errorf("tailer still registered after closeTask")
	}
}

// TestTailer_DetachDoesNotStopTailer: refCount dropping to 0 should NOT kill
// the tailer (silent mode keeps buffering for late joiners until task_done
// or idle grace). Regression guard against accidentally "reference-counting
// → close" which was a tempting simplification.
func TestTailer_DetachDoesNotStopTailer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeJSONL(t, dir, "x", false)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	tl, _ := r.ensureTailer("k", "t1", "toolu", path)
	c, _ := newCapturedClient(t, nil)
	r.attach(tailerKey{"k", "t1"}, c)
	r.detach(tailerKey{"k", "t1"}, c)

	// Ref count back to 0; tailer still registered.
	if got := tl.refCount.Load(); got != 0 {
		t.Errorf("refCount=%d after detach, want 0", got)
	}
	r.mu.RLock()
	_, stillThere := r.byTask[tailerKey{"k", "t1"}]
	r.mu.RUnlock()
	if !stillThere {
		t.Errorf("tailer evicted on refCount=0 (should stay until task_done)")
	}
}

// TestTailer_DetachClientDropsAllSubscriptions: on WS disconnect, the
// registry must drop every subscription the client held so stale pointers
// don't linger in `subs` maps.
func TestTailer_DetachClientDropsAllSubscriptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p1 := writeJSONL(t, dir, "one", false)
	p2 := filepath.Join(dir, "agent-b.jsonl")
	os.WriteFile(p2, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`+"\n"), 0o644)

	r := newTailerRegistry(nil)
	defer r.Shutdown()

	r.ensureTailer("k", "t1", "u", p1)
	r.ensureTailer("k", "t2", "u", p2)
	c, _ := newCapturedClient(t, nil)
	r.attach(tailerKey{"k", "t1"}, c)
	r.attach(tailerKey{"k", "t2"}, c)

	r.detachClient(c)

	for _, tk := range []tailerKey{{"k", "t1"}, {"k", "t2"}} {
		r.mu.RLock()
		tl := r.byTask[tk]
		r.mu.RUnlock()
		if tl == nil {
			t.Errorf("tailer %v missing", tk)
			continue
		}
		tl.mu.Lock()
		_, still := tl.subs[c]
		tl.mu.Unlock()
		if still {
			t.Errorf("client still in tailer subs for %v", tk)
		}
	}
	r.mu.RLock()
	_, tracked := r.clientSubs[c]
	r.mu.RUnlock()
	if tracked {
		t.Errorf("reverse index still has detached client")
	}
}

// ─── helpers ────────────────────────────────────────────────────────────

func drainChan(out <-chan node.ServerMsg, dur time.Duration) {
	deadline := time.After(dur)
	for {
		select {
		case <-out:
		case <-deadline:
			return
		}
	}
}

// itoaSmall without fmt to keep this file lean (fmt import is already allowed,
// but this is a micro-helper that's easier to read inline).
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
