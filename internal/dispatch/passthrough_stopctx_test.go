package dispatch

import (
	"context"
	"testing"
	"time"
)

// TestMergeStopAndValues pins the merged-ctx contract (#1320): Done / Err /
// Deadline follow cancelSrc (the service ctx, so SIGTERM reaches an in-flight
// send) and never valuesSrc (the webhook ctx, which ends within seconds);
// Value prefers valuesSrc and falls back to cancelSrc; nil parents degrade
// instead of panicking (R260528-GO-11). The end-to-end wiring is covered by
// TestDetachedTurn_CtxOutlivesInboundAndFollowsStop.
func TestMergeStopAndValues(t *testing.T) {
	t.Parallel()
	type keyStop struct{}
	type keyValues struct{}
	type keyBoth struct{}

	stopDeadline := time.Now().Add(time.Hour)
	stopCtx, stopCancel := context.WithDeadline(context.Background(), stopDeadline)
	defer stopCancel()
	stopCtx = context.WithValue(stopCtx, keyStop{}, "from-stop")
	stopCtx = context.WithValue(stopCtx, keyBoth{}, "stop")
	valuesCtx, valuesCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer valuesCancel()
	valuesCtx = context.WithValue(valuesCtx, keyValues{}, "from-values")
	valuesCtx = context.WithValue(valuesCtx, keyBoth{}, "values")

	merged := mergeStopAndValues(stopCtx, valuesCtx)

	if d, ok := merged.Deadline(); !ok || !d.Equal(stopDeadline) {
		t.Errorf("Deadline = (%v, %v), want (%v, true) from cancelSrc", d, ok, stopDeadline)
	}
	for key, want := range map[any]any{keyValues{}: "from-values", keyStop{}: "from-stop", keyBoth{}: "values", "missing": nil} {
		if got := merged.Value(key); got != want {
			t.Errorf("Value(%T) = %v, want %v", key, got, want)
		}
	}

	valuesCancel()
	select {
	case <-merged.Done():
		t.Fatal("Done fired on valuesSrc cancel; must follow cancelSrc only")
	case <-time.After(20 * time.Millisecond):
	}
	stopCancel()
	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("Done did not fire on cancelSrc cancel")
	}
	if merged.Err() == nil {
		t.Error("Err() = nil after cancelSrc canceled")
	}

	//lint:ignore SA1012 intentional nil ctx: pins the defensive nil-ctx fallback
	if err := mergeStopAndValues(nil, context.Background()).Err(); err != nil {
		t.Errorf("nil cancelSrc: Err() = %v, want nil (Background fallback)", err)
	}
	if got := mergeStopAndValues(context.Background(), nil).Value("anything"); got != nil {
		t.Errorf("nil valuesSrc: Value = %v, want nil", got)
	}
}
