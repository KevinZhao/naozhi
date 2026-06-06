package cli

import (
	"sync"
	"testing"
)

// TestReadEvent_PoolNoCrossFrameBleed is the regression test for
// R220123-PERF-13 (#1637). ReadEvent now unmarshals into a pooled *Event;
// the pooled struct MUST be reset before re-entering the pool so a later
// frame's parse never inherits a prior frame's pointer fields. A result
// event parsed right after an assistant event must carry a nil Message —
// if the pool leaked the prior assistant's Message pointer the dashboard
// would render a phantom content block.
func TestReadEvent_PoolNoCrossFrameBleed(t *testing.T) {
	p := &ClaudeProtocol{}

	assistant := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
	ev1, _, err := p.ReadEvent(assistant)
	if err != nil {
		t.Fatalf("assistant ReadEvent err: %v", err)
	}
	if len(ev1) != 1 || ev1[0].Message == nil || len(ev1[0].Message.Content) != 1 {
		t.Fatalf("assistant event did not parse Message content: %+v", ev1)
	}

	// A result frame has no message field. If the pooled struct were not
	// reset, ev2[0].Message would alias the assistant's Message.
	result := `{"type":"result","total_cost_usd":0.01}`
	ev2, done, err := p.ReadEvent(result)
	if err != nil {
		t.Fatalf("result ReadEvent err: %v", err)
	}
	if !done {
		t.Errorf("result event should report done=true")
	}
	if len(ev2) != 1 {
		t.Fatalf("expected 1 result event, got %d", len(ev2))
	}
	if ev2[0].Message != nil {
		t.Fatalf("pool bleed: result event inherited a Message pointer from a prior frame: %+v", ev2[0].Message)
	}
	if ev2[0].Type != "result" {
		t.Errorf("type = %q, want result", ev2[0].Type)
	}

	// The first event's copy must remain intact after the pooled header was
	// recycled by the second parse — proves the success path copies the
	// value out rather than returning the live pooled pointer.
	if ev1[0].Message == nil || ev1[0].Message.Content[0].Text != "hi" {
		t.Fatalf("returned event mutated after pool reuse: %+v", ev1[0])
	}
}

// TestReadEvent_ReturnedEventIndependentOfPool parses many frames in a row
// and keeps every returned slice; if the pool handed back the live struct
// (instead of a value copy) later parses would corrupt earlier results.
func TestReadEvent_ReturnedEventIndependentOfPool(t *testing.T) {
	p := &ClaudeProtocol{}
	const n = 50
	got := make([][]Event, 0, n)
	for i := 0; i < n; i++ {
		line := `{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"frame"}]}}`
		ev, _, err := p.ReadEvent(line)
		if err != nil {
			t.Fatalf("frame %d err: %v", i, err)
		}
		got = append(got, ev)
	}
	for i, ev := range got {
		if len(ev) != 1 || ev[0].Type != "assistant" {
			t.Fatalf("frame %d corrupted: %+v", i, ev)
		}
		if ev[0].Message == nil || ev[0].Message.Content[0].Text != "frame" {
			t.Fatalf("frame %d content corrupted by pool reuse: %+v", i, ev[0])
		}
	}
}

// TestReadEvent_PoolConcurrentSafe exercises the shared pool from many
// goroutines so `go test -race` flags any data race in the Get/Reset/Put
// cycle. The pool is package-global, so concurrent ReadEvent callers
// (multiple session readLoops) must not corrupt each other's frames.
func TestReadEvent_PoolConcurrentSafe(t *testing.T) {
	p := &ClaudeProtocol{}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ev, _, err := p.ReadEvent(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}`)
				if err != nil {
					t.Errorf("concurrent ReadEvent err: %v", err)
					return
				}
				if len(ev) != 1 || ev[0].Message == nil || ev[0].Message.Content[0].Text != "x" {
					t.Errorf("concurrent frame corrupted: %+v", ev)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestReadEvent_SkipPathRecyclesPool ensures the early skip returns (hook /
// control_response) still return the pooled struct without leaking it or
// bleeding into the next parse. We can't observe the pool directly, but a
// subsequent genuine parse after a run of skips must be correct.
func TestReadEvent_SkipPathRecyclesPool(t *testing.T) {
	p := &ClaudeProtocol{}
	skips := []string{
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"control_response"}`,
		`{"type":"system","subtype":"hook_response"}`,
	}
	for _, s := range skips {
		ev, done, err := p.ReadEvent(s)
		if err != nil {
			t.Fatalf("skip ReadEvent(%s) err: %v", s, err)
		}
		if ev != nil || done {
			t.Fatalf("skip ReadEvent(%s) = (%v, %v), want (nil, false)", s, ev, done)
		}
	}
	ev, _, err := p.ReadEvent(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"after"}]}}`)
	if err != nil {
		t.Fatalf("post-skip ReadEvent err: %v", err)
	}
	if len(ev) != 1 || ev[0].Message == nil || ev[0].Message.Content[0].Text != "after" {
		t.Fatalf("post-skip parse corrupted by skip-path pool handling: %+v", ev)
	}
}

// BenchmarkReadEvent measures the per-frame allocation cost of the hot
// shim-stdout ingest path. R220123-PERF-13 (#1637): the pooled *Event
// removes the per-frame Event-header heap allocation; this benchmark is the
// alloc-count guard the ticket asked for. Run with `-benchmem`; the
// header allocation should no longer appear in allocs/op.
func BenchmarkReadEvent(b *testing.B) {
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"hello world"}]}}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev, _, err := p.ReadEvent(line)
		if err != nil || len(ev) != 1 {
			b.Fatalf("ReadEvent err=%v len=%d", err, len(ev))
		}
	}
}

// TestReadEventInto_ParityAndBufReuse pins R20260603-PERF-10 (#1676):
// ReadEventInto must (a) return the same Event/done/err as ReadEvent, and
// (b) back the returned slice with the caller-supplied buf so the single-event
// hot path reuses the array instead of allocating a fresh one per frame.
func TestReadEventInto_ParityAndBufReuse(t *testing.T) {
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`

	want, wantDone, wantErr := p.ReadEvent(line)
	if wantErr != nil || len(want) != 1 {
		t.Fatalf("ReadEvent baseline err=%v len=%d", wantErr, len(want))
	}

	var arr [2]Event
	buf := arr[:0]
	got, done, err := p.ReadEventInto(line, buf)
	if err != nil {
		t.Fatalf("ReadEventInto err=%v", err)
	}
	if done != wantDone {
		t.Errorf("done = %v, want %v", done, wantDone)
	}
	if len(got) != 1 || got[0].Type != "assistant" ||
		got[0].Message == nil || got[0].Message.Content[0].Text != "hi" {
		t.Fatalf("ReadEventInto parity mismatch: %+v", got)
	}
	// The returned slice must alias the caller's array (no fresh backing alloc).
	if &got[0] != &arr[0] {
		t.Error("ReadEventInto returned slice not backed by caller buf")
	}

	// A skip frame returns nil without touching buf.
	skip := `{"type":"control_response"}`
	if ev, _, err := p.ReadEventInto(skip, arr[:0]); err != nil || ev != nil {
		t.Errorf("skip ReadEventInto = (%v, %v), want (nil, nil)", ev, err)
	}
}

// BenchmarkReadEventInto guards the zero-extra-alloc claim: reusing a single
// backing array across frames must not allocate the 1-element slice header
// that ReadEvent pays each call. Run with -benchmem.
func BenchmarkReadEventInto(b *testing.B) {
	p := &ClaudeProtocol{}
	line := `{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"hello world"}]}}`
	var arr [2]Event
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev, _, err := p.ReadEventInto(line, arr[:0])
		if err != nil || len(ev) != 1 {
			b.Fatalf("ReadEventInto err=%v len=%d", err, len(ev))
		}
	}
}
