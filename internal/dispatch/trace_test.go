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
	trace := func(ctx context.Context, msg platform.IncomingMessage) string {
		return ctxutil.TraceID(withInboundTrace(ctx, msg))
	}
	if got := trace(context.Background(), platform.IncomingMessage{Platform: "feishu", EventID: "ev1"}); got != "feishu:ev1" {
		t.Fatalf("trace from event id = %q, want feishu:ev1", got)
	}
	// Control bytes from the platform payload never reach a log field.
	if got := trace(context.Background(), platform.IncomingMessage{Platform: "slack\n", EventID: "evt-1\x1b[0m"}); got != "slack_:evt-1_[0m" {
		t.Fatalf("sanitized trace id = %q", got)
	}
	// No event id: a fresh id, never empty.
	if got := trace(context.Background(), platform.IncomingMessage{Platform: "feishu"}); len(got) != 16 {
		t.Fatalf("minted trace id = %q", got)
	}
	// An upstream trace wins.
	if got := trace(ctxutil.WithTraceID(context.Background(), "outer"), platform.IncomingMessage{Platform: "feishu", EventID: "e"}); got != "outer" {
		t.Fatalf("existing trace overwritten: %q", got)
	}
}

// A message's "message received", "message replied" and (for a failed turn)
// "turn ended in failure" lines carry the same trace_id, a run_id and the
// session_key, so journalctl can be grepped by any of the three (#3436). The logger is the one BuildHandler's path uses:
// prepareInbound builds it from slog.Default, so the test swaps the default.
func TestIMTurn_LogsCarryCorrelation(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(ctxutil.NewHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var seen struct {
		run, key, trace string
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSendFn(func(ctx context.Context, _ string, _ turn.Session, _ string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
		seen.run, seen.key, seen.trace = ctxutil.RunID(ctx), ctxutil.SessionKey(ctx), ctxutil.TraceID(ctx)
		return &clievent.SendResult{Text: "pong", IsError: true}, nil
	}))
	// The test router has no CLI; InjectSession gives the key a live process.
	router := d.router.(*session.Router)
	const key = "fake:direct:chat1:general"
	router.InjectSession(key, session.NewTestProcess())

	d.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: "fake", EventID: "evt-corr", UserID: "user1", ChatID: "chat1", ChatType: "direct", Text: "ping",
	})

	if seen.run == "" || seen.key != key || seen.trace != "fake:evt-corr" {
		t.Fatalf("Send ctx carried run=%q key=%q trace=%q", seen.run, seen.key, seen.trace)
	}
	var received, replied, failed map[string]any
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
		case "turn ended in failure":
			failed = m
		}
	}
	if received == nil || replied == nil || failed == nil {
		t.Fatalf("missing log lines in:\n%s", buf.String())
	}
	if received["trace_id"] != "fake:evt-corr" || replied["trace_id"] != "fake:evt-corr" {
		t.Errorf("trace_id: received=%v replied=%v", received["trace_id"], replied["trace_id"])
	}
	if replied["run_id"] != seen.run || replied["session_key"] != key {
		t.Errorf("replied line run_id=%v session_key=%v, want %q / %q", replied["run_id"], replied["session_key"], seen.run, key)
	}
	if failed["run_id"] != seen.run || failed["trace_id"] != "fake:evt-corr" {
		t.Errorf("failure line run_id=%v trace_id=%v, want %q", failed["run_id"], failed["trace_id"], seen.run)
	}
}
