package cli

// lineBufShrinkThreshold tuning (R221-PERF-P2-5).
//
// The readLoop's lineBuf carries capacity across iterations: a large
// event grows the backing array; most of the time the next event is
// small and we want to reuse the grown capacity instead of paying
// realloc(4→8→16→32…→target) again. We shrink only when the retained
// capacity gets large enough to be a memory concern. The threshold
// tradeoff is documented in process.go:48.
//
// These tests pin two behaviours:
//
//   1. The constant's value is 256 KiB. A code change that lowers the
//      threshold back into the 50-200 KiB "common large event" band
//      re-introduces the realloc churn this constant was raised to
//      eliminate; making that change requires updating this test and
//      forces the author to see the rationale.
//
//   2. readLoop correctly forwards a stream of mid-large (>64 KiB,
//      <256 KiB) events — the band that used to trip shrinkage on
//      every iteration and now should retain capacity. We send 10
//      back-to-back 150 KiB events and assert all 10 are delivered
//      intact to p.eventCh without truncation or frame merging.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestLineBufShrinkThreshold_ConstantValue locks in the current value.
// It is a weak test — the constant can be changed — but any change
// requires updating this test, which in turn forces whoever is
// lowering the bar to re-read the rationale comment above the const
// and justify the regression.
func TestLineBufShrinkThreshold_ConstantValue(t *testing.T) {
	const want = 256 * 1024
	if lineBufShrinkThreshold != want {
		t.Errorf("lineBufShrinkThreshold = %d, want %d — see process.go:48 rationale before changing",
			lineBufShrinkThreshold, want)
	}
}

// TestReadLoop_LargeEventsRetainCapacity drives 10 consecutive events
// just above the OLD threshold (64 KiB) but below the NEW one (256
// KiB). On the previous threshold, each event forced a shrink-and-
// regrow; behaviourally this test passed then too, because shrinking
// is a performance-only operation. The test's value is as a future
// regression guard: if someone refactors readLoop and breaks mid-large
// event handling outright (e.g. a lineBuf = nil typo, a scanner buffer
// misconfig), this shows up here instead of in production.
//
// Event size: 150 KiB of JSON-escaped filler inside an assistant
// message's text field. That puts the total line payload around
// 150 KiB + ~100 bytes of envelope — comfortably into the "retain
// capacity" band under the new 256 KiB threshold, and comfortably
// above the bufio default 4 KiB so the inner ReadSlice loop is
// exercised rather than taking the single-chunk fast path.
func TestReadLoop_LargeEventsRetainCapacity(t *testing.T) {
	t.Parallel()
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()
	defer p.Kill()

	const (
		eventCount = 10
		payloadLen = 150 * 1024 // 150 KiB — between old 64 KiB and new 256 KiB thresholds
	)
	filler := strings.Repeat("x", payloadLen)

	for i := 0; i < eventCount; i++ {
		// Build a valid assistant event. json.Marshal handles the
		// escaping so the filler's raw bytes survive into .Text.
		ev := map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": filler},
				},
			},
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		srv.SendStdout(string(raw))
	}
	// Signal end-of-stream so readLoop exits and closes p.eventCh.
	srv.SendCLIExited(0)

	// Collect with a hard deadline so a hang surfaces here rather than
	// waiting for the go-test -timeout to fire minutes later.
	timeout := time.After(10 * time.Second)
	var got int
	for {
		select {
		case ev, ok := <-p.eventCh:
			if !ok {
				if got != eventCount {
					t.Errorf("received %d events, want %d", got, eventCount)
				}
				return
			}
			if ev.Type != "assistant" {
				t.Errorf("event %d type = %q, want assistant", got, ev.Type)
			}
			got++
		case <-timeout:
			t.Fatalf("timeout waiting for events; received %d of %d", got, eventCount)
		}
	}
}
