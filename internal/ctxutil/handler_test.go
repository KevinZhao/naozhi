package ctxutil

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// records decodes one JSON object per line.
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		recs = append(recs, m)
	}
	return recs
}

func TestHandler_AddsCorrelationFromCtx(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := WithSessionKey(WithRunID(WithTraceID(context.Background(), "t1"), "r1"), "k1")

	lg.InfoContext(ctx, "with ctx", "x", 1)
	lg.Info("without ctx")
	lg.With("pre", "set").WarnContext(WithRunID(context.Background(), "r2"), "with attrs")

	recs := records(t, &buf)
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

// A field the call site or a derived logger (LoggerWithTrace, cron's
// explicit run_id) already set is not written a second time.
func TestHandler_NoDuplicateField(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := WithRunID(WithTraceID(context.Background(), "ctx-trace"), "ctx-run")

	lg.InfoContext(ctx, "record attr", "run_id", "own-run")
	LoggerWithTrace(WithTraceID(context.Background(), "with-trace"), lg).InfoContext(ctx, "derived")

	out := buf.String()
	if n := strings.Count(out, `"run_id"`); n != 2 {
		t.Fatalf("run_id written %d times, want once per record:\n%s", n, out)
	}
	if n := strings.Count(out, `"trace_id"`); n != 2 {
		t.Fatalf("trace_id written %d times, want once per record:\n%s", n, out)
	}
	recs := records(t, &buf)
	if recs[0]["run_id"] != "own-run" || recs[0]["trace_id"] != "ctx-trace" {
		t.Fatalf("record attr = %v", recs[0])
	}
	if recs[1]["trace_id"] != "with-trace" || recs[1]["run_id"] != "ctx-run" {
		t.Fatalf("derived = %v", recs[1])
	}
}

func TestHandler_EnabledAndGroupDelegate(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("Enabled must defer to the wrapped handler's level")
	}
	if h.WithGroup("") != slog.Handler(h) {
		t.Fatal(`WithGroup("") must return the handler unchanged`)
	}
	g, ok := h.WithGroup("g").(*Handler)
	if !ok {
		t.Fatal("WithGroup must keep the wrapper")
	}
	slog.New(g).WarnContext(WithRunID(context.Background(), "r"), "grouped")
	recs := records(t, &buf)
	grp, _ := recs[0]["g"].(map[string]any)
	if grp["run_id"] != "r" {
		t.Fatalf("grouped record = %v", recs[0])
	}
}
