package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

func TestWithInboundTrace(t *testing.T) {
	t.Parallel()
	ctx := withInboundTrace(context.Background(), platform.IncomingMessage{EventID: "evt-1\x1b[0m"})
	if got := ctxutil.TraceID(ctx); got != "evt-1" && got == "" {
		t.Fatalf("trace from event id = %q", got)
	}
	if got := ctxutil.TraceID(ctx); got == "" || len(got) > 64 {
		t.Fatalf("sanitized trace id = %q", got)
	}
	// No event id: a fresh id, never empty.
	if got := ctxutil.TraceID(withInboundTrace(context.Background(), platform.IncomingMessage{})); len(got) != 16 {
		t.Fatalf("minted trace id = %q", got)
	}
	// An upstream trace wins.
	pre := ctxutil.WithTraceID(context.Background(), "outer")
	if got := ctxutil.TraceID(withInboundTrace(pre, platform.IncomingMessage{EventID: "e"})); got != "outer" {
		t.Fatalf("existing trace overwritten: %q", got)
	}
}

// A message's "message received" and "message replied" lines carry the same
// trace_id, a run_id and the session_key, so journalctl can be grepped by
// any of the three (#3436). The logger is the one BuildHandler's path uses:
// prepareInbound builds it from slog.Default, so the test swaps the default.
func TestIMTurn_LogsCarryCorrelation(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(ctxutil.NewHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var seen struct {
		run, key string
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSendFn(func(ctx context.Context, _ string, _ turn.Session, _ string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
		seen.run, seen.key = ctxutil.RunID(ctx), ctxutil.SessionKey(ctx)
		return &clievent.SendResult{Text: "pong"}, nil
	}))
	// The test router has no CLI; InjectSession gives the key a live process.
	router := d.router.(*session.Router)
	const key = "fake:direct:chat1:general"
	router.InjectSession(key, session.NewTestProcess())

	d.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: "fake", EventID: "evt-corr", UserID: "user1", ChatID: "chat1", ChatType: "direct", Text: "ping",
	})

	if seen.run == "" || seen.key != key {
		t.Fatalf("Send ctx carried run=%q key=%q", seen.run, seen.key)
	}
	var received, replied map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m["msg"] {
		case "message received":
			received = m
		case "message replied":
			replied = m
		}
	}
	if received == nil || replied == nil {
		t.Fatalf("missing log lines in:\n%s", buf.String())
	}
	if received["trace_id"] != "evt-corr" || replied["trace_id"] != "evt-corr" {
		t.Errorf("trace_id: received=%v replied=%v", received["trace_id"], replied["trace_id"])
	}
	if replied["run_id"] != seen.run || replied["session_key"] != key {
		t.Errorf("replied line run_id=%v session_key=%v, want %q / %q", replied["run_id"], replied["session_key"], seen.run, key)
	}
}
