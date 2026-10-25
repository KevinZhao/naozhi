package ctxutil

import (
	"context"
	"testing"
)

// TestWithTraceID_RoundTrip verifies the standard ingress path: middleware
// stamps a trace id, downstream code reads it back. A trivial test that
// would catch a future refactor swapping the unexported key by accident.
func TestWithTraceID_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := WithTraceID(context.Background(), "abc123")
	if got := TraceID(ctx); got != "abc123" {
		t.Fatalf("TraceID(WithTraceID(_, abc123)) = %q; want abc123", got)
	}
}

// TestWithTraceID_EmptyIsNoop documents the contract that an empty id
// must not pollute the context. Otherwise the trace-aware slog Handler
// would emit `trace_id=""` for every untraced request, defeating the
// log-grep that motivated the feature.
func TestWithTraceID_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	derived := WithTraceID(parent, "")
	if derived != parent {
		t.Fatalf("WithTraceID(_, \"\") should return the parent ctx unchanged")
	}
	if got := TraceID(derived); got != "" {
		t.Fatalf("TraceID on empty-id ctx = %q; want \"\"", got)
	}
}

// TestTraceID_NilCtx — the helper is reached from defensive logging
// paths that may be called before a request context exists; a nil ctx
// must not panic.
func TestTraceID_NilCtx(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("TraceID(nil) panicked: %v", r)
		}
	}()
	//lint:ignore SA1012 intentional nil ctx: pins the defensive nil-ctx fallback under test
	if got := TraceID(nil); got != "" {
		t.Fatalf("TraceID(nil) = %q; want \"\"", got)
	}
}

// TestNewTraceID_HexAnd16Chars verifies the rendered shape; a future
// refactor that swaps to base64 or shrinks the entropy would break
// log-grep tooling that locks onto the 16-hex pattern.
func TestNewTraceID_HexAnd16Chars(t *testing.T) {
	t.Parallel()
	id := NewTraceID()
	if len(id) != 16 {
		t.Fatalf("NewTraceID() = %q (len %d); want 16-char hex", id, len(id))
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			t.Fatalf("NewTraceID() contains non-hex byte %q in %q", r, id)
		}
	}
	// Two consecutive ids should be distinct (collision probability
	// is 2^-64; any failure here means crypto/rand is broken).
	if NewTraceID() == id {
		t.Fatalf("two consecutive NewTraceID() calls returned the same value %q", id)
	}
}
