package dispatch

import (
	"context"
	"testing"
	"time"
)

// TestMergeStopAndValues_DoneFollowsCancelSrc pins the contract that the
// merged context's Done channel is driven by the cancel source — not the
// values source. This is the load-bearing property: webhook-handler ctx
// (the values source) returns within seconds and Cancel-fires on its own
// timer; without this routing the passthrough goroutine would abort the
// instant the webhook returned, defeating the entire detach. (#1320)
func TestMergeStopAndValues_DoneFollowsCancelSrc(t *testing.T) {
	t.Parallel()
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	valuesCtx, valuesCancel := context.WithCancel(context.Background())

	merged := mergeStopAndValues(stopCtx, valuesCtx)

	// values cancellation must NOT propagate (this is the property
	// WithoutCancel had and we must preserve it for webhook-handler ctxs).
	valuesCancel()
	select {
	case <-merged.Done():
		t.Fatal("merged.Done() fired on valuesSrc cancel; should only fire on cancelSrc")
	case <-time.After(20 * time.Millisecond):
	}

	// cancelSrc cancellation MUST propagate — this is the new behavior
	// vs WithoutCancel: SIGTERM on the service ctx should reach the
	// passthrough goroutine.
	stopCancel()
	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("merged.Done() did not fire on cancelSrc cancel")
	}
	if err := merged.Err(); err == nil {
		t.Error("merged.Err() = nil after cancelSrc canceled, want non-nil")
	}
}

// TestMergeStopAndValues_ValueLookupFromValuesSrcFirst confirms that
// per-request slog attrs / auth values attached to the webhook ctx
// remain reachable through the merged ctx. Without this, sendAndReply's
// downstream value lookups (request_id, planner trace, etc.) would
// silently fail after the merge. The fallback chain still consults
// cancelSrc so service-ctx-attached values (cron / sysession trace IDs)
// remain reachable too.
func TestMergeStopAndValues_ValueLookupFromValuesSrcFirst(t *testing.T) {
	t.Parallel()
	type ctxKeyA struct{}
	type ctxKeyB struct{}

	stopCtx := context.WithValue(context.Background(), ctxKeyA{}, "from-stop")
	valuesCtx := context.WithValue(context.Background(), ctxKeyB{}, "from-values")

	merged := mergeStopAndValues(stopCtx, valuesCtx)

	if got := merged.Value(ctxKeyB{}); got != "from-values" {
		t.Errorf("merged.Value(B) = %v, want from-values", got)
	}
	if got := merged.Value(ctxKeyA{}); got != "from-stop" {
		t.Errorf("merged.Value(A) = %v, want from-stop (cancelSrc fallback)", got)
	}
	if got := merged.Value("missing"); got != nil {
		t.Errorf("merged.Value(missing) = %v, want nil", got)
	}
}

// TestMergeStopAndValues_ValuesSrcWins pins the precedence: when both
// parents define the same key, valuesSrc wins. This matters because the
// webhook handler may attach a per-request override (e.g. a request-scoped
// logger) on top of a service-wide default; the merged ctx must surface
// the per-request value, not the service default.
func TestMergeStopAndValues_ValuesSrcWins(t *testing.T) {
	t.Parallel()
	type k struct{}
	stopCtx := context.WithValue(context.Background(), k{}, "stop")
	valuesCtx := context.WithValue(context.Background(), k{}, "values")
	merged := mergeStopAndValues(stopCtx, valuesCtx)
	if got := merged.Value(k{}); got != "values" {
		t.Errorf("merged.Value = %v, want 'values' (valuesSrc must win)", got)
	}
}

// TestMergeStopAndValues_NilValuesSrcAllowed exercises the documented
// nil-values-source branch. Production-style callers always pass a real
// webhook ctx, but the helper must handle nil to keep test wiring simple
// (and to defang the "valuesSrc nil" panic surface).
func TestMergeStopAndValues_NilValuesSrcAllowed(t *testing.T) {
	t.Parallel()
	stopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	merged := mergeStopAndValues(stopCtx, nil)
	if merged.Done() == nil {
		t.Error("merged.Done() returned nil channel")
	}
	if got := merged.Value("anything"); got != nil {
		t.Errorf("merged.Value(missing) = %v, want nil", got)
	}
}

// TestMergeStopAndValues_NilCancelSrcFallsBackToBackground pins the
// R260528-GO-11 degraded-fallback contract. Pre-fix, nil cancelSrc
// panicked — but NewDispatcher always seeds stopCtx so the panic was
// dead code; if a future code path ever does pass nil, we'd rather
// surface a slog.Error and degrade to context.Background() than crash
// the dispatcher mid-flight. The dispatcher_stopctx_wireup_test.go
// boot-time check still guarantees production wiring is non-nil.
func TestMergeStopAndValues_NilCancelSrcFallsBackToBackground(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil cancelSrc must NOT panic post-R260528-GO-11; got panic=%v", r)
		}
	}()
	merged := mergeStopAndValues(nil, context.Background())
	if merged == nil {
		t.Fatal("merged ctx must be non-nil even with nil cancelSrc")
	}
	// context.Background()'s Done() returns nil and Err() returns nil
	// — both valid signals meaning "never cancels". Just confirm Err
	// is nil so the in-flight send proceeds rather than aborting.
	if err := merged.Err(); err != nil {
		t.Errorf("merged.Err() = %v, want nil (Background never cancels)", err)
	}
}

// TestMergeStopAndValues_DeadlineFromCancelSrc verifies the merged ctx
// reports the cancel source's deadline. The webhook ctx may have its own
// 30-second handler deadline, but once detached we want the passthrough
// goroutine to see the long-lived service ctx's "no deadline" rather
// than inherit the handler-side deadline.
func TestMergeStopAndValues_DeadlineFromCancelSrc(t *testing.T) {
	t.Parallel()
	stopDeadline := time.Now().Add(time.Hour)
	stopCtx, cancel := context.WithDeadline(context.Background(), stopDeadline)
	defer cancel()
	valuesCtx, vcancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer vcancel()
	merged := mergeStopAndValues(stopCtx, valuesCtx)
	d, ok := merged.Deadline()
	if !ok {
		t.Fatal("merged.Deadline ok = false, want true (from cancelSrc)")
	}
	if !d.Equal(stopDeadline) {
		t.Errorf("merged.Deadline = %v, want %v (from cancelSrc, not valuesSrc)", d, stopDeadline)
	}
}
