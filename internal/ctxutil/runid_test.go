package ctxutil

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestRunIDAndSessionKey_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var nilCtx context.Context
	if RunID(ctx) != "" || SessionKey(ctx) != "" || RunID(nilCtx) != "" {
		t.Fatal("empty ctx should carry nothing")
	}
	if WithRunID(ctx, "") != ctx || WithSessionKey(ctx, "") != ctx {
		t.Fatal("empty values must not allocate a new ctx")
	}
	ctx = WithSessionKey(WithRunID(ctx, "r1"), "feishu:direct:a:general")
	if RunID(ctx) != "r1" || SessionKey(ctx) != "feishu:direct:a:general" {
		t.Fatalf("got %q / %q", RunID(ctx), SessionKey(ctx))
	}
}

func TestHandler_AddsCorrelationFromCtx(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := WithSessionKey(WithRunID(WithTraceID(context.Background(), "t1"), "r1"), "k1")

	lg.InfoContext(ctx, "with ctx", "x", 1)
	lg.Info("without ctx")
	lg.With("pre", "set").WarnContext(WithRunID(context.Background(), "r2"), "with attrs")

	var recs []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, m)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %d", len(recs))
	}
	if recs[0]["trace_id"] != "t1" || recs[0]["run_id"] != "r1" || recs[0]["session_key"] != "k1" || recs[0]["x"] != float64(1) {
		t.Fatalf("ctx record = %v", recs[0])
	}
	for _, k := range []string{"trace_id", "run_id", "session_key"} {
		if _, ok := recs[1][k]; ok {
			t.Fatalf("ctx-less record carries %s: %v", k, recs[1])
		}
	}
	if recs[2]["pre"] != "set" || recs[2]["run_id"] != "r2" || recs[2]["trace_id"] != nil {
		t.Fatalf("WithAttrs record = %v", recs[2])
	}
}
