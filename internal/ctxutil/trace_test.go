package ctxutil

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
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
// must not pollute the context. Otherwise downstream LoggerWithTrace
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
	if got := TraceID(nil); got != "" {
		t.Fatalf("TraceID(nil) = %q; want \"\"", got)
	}
}

// TestLoggerWithTrace_AddsField is the load-bearing observability
// contract: a logger derived from a traced ctx must emit trace_id on
// every record it produces, and the field must show up in the standard
// slog JSON output.
func TestLoggerWithTrace_AddsField(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := WithTraceID(context.Background(), "trace-xyz")
	derived := LoggerWithTrace(ctx, base)
	derived.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("invalid JSON log record: %v\n%s", err, buf.String())
	}
	if rec["trace_id"] != "trace-xyz" {
		t.Fatalf("log record missing trace_id field: %v", rec)
	}
}

// TestLoggerWithTrace_NoTraceNoField protects against a regression
// that would always inject a trace_id field, polluting every log line
// in code paths that have no request context.
func TestLoggerWithTrace_NoTraceNoField(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	derived := LoggerWithTrace(context.Background(), base)
	derived.Info("hello")
	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("untraced ctx must not emit trace_id field: %s", buf.String())
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
