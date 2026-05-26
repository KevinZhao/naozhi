package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/slack-go/slack/slackevents"
)

var _ platform.RunnablePlatform = (*Slack)(nil)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	if s.Name() != "slack" {
		t.Errorf("Name() = %q, want slack", s.Name())
	}
	if s.MaxReplyLength() != 4000 {
		t.Errorf("MaxReplyLength() = %d, want 4000", s.MaxReplyLength())
	}
}

func TestNew_CustomMaxReplyLen(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test", MaxReplyLen: 2000})
	if s.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", s.MaxReplyLength())
	}
}

func TestStartAlreadyStarted(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.startMu.Lock()
	s.started = true
	s.startMu.Unlock()
	noop := func(_ context.Context, _ platform.IncomingMessage) {}
	err := s.Start(noop)
	if err == nil {
		t.Error("expected error for double Start()")
	}
}

func TestStopNoop(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	if err := s.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestEditMessage_InvalidFormat(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	err := s.EditMessage(context.Background(), "no-colon-here", "text")
	if err == nil {
		t.Error("expected error for invalid msgID format")
	}
}

func TestHandleMessage_BotMessage(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	called := false
	s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	s.handleMessage(&slackevents.MessageEvent{BotID: "B123", Text: "hello"})
	if called {
		t.Error("bot messages should be skipped")
	}
}

func TestHandleMessage_SubType(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	called := false
	s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	s.handleMessage(&slackevents.MessageEvent{SubType: "message_changed", Text: "hello"})
	if called {
		t.Error("subtype messages should be skipped")
	}
}

func TestHandleMessage_MentionStrip(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.botID = "U123"
	var received platform.IncomingMessage
	done := make(chan struct{})
	s.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	s.handleMessage(&slackevents.MessageEvent{
		User:        "U456",
		Channel:     "C789",
		ChannelType: "channel",
		Text:        "<@U123> hello bot",
		TimeStamp:   "1234567890.000100",
	})
	<-done
	if received.Text != "hello bot" {
		t.Errorf("Text = %q, want 'hello bot'", received.Text)
	}
	if !received.MentionMe {
		t.Error("MentionMe should be true")
	}
	if received.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", received.ChatType)
	}
}

func TestHandleMessage_DirectMessage(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	var received platform.IncomingMessage
	done := make(chan struct{})
	s.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	s.handleMessage(&slackevents.MessageEvent{
		User: "U456", Channel: "D789", ChannelType: "im",
		Text: "hello", TimeStamp: "1234567890.000200",
	})
	<-done
	if received.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct", received.ChatType)
	}
}

func TestHandleMessage_EmptyAfterMentionStrip(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.botID = "U123"
	called := false
	s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	s.handleMessage(&slackevents.MessageEvent{Text: "<@U123>"})
	if called {
		t.Error("empty text after mention strip should be skipped")
	}
}

// TestNoSigningSecretSurface pins the architectural fact that the Slack
// adapter exposes NO HTTP webhook endpoint — it is a Socket Mode adapter
// only. Issue #879 (R244-SEC-P1-4) flagged that the feishu transport_hook
// has a signingSecret == "" / token-only fallback that was historically
// replay-vulnerable; the pin test there asks: does Slack share that risk?
//
// The answer is "no, by construction": Slack inbound traffic arrives on a
// websocket the slack-go SDK opens to https://wss-primary.slack.com/ using
// the AppToken (xapp-…). Slack itself authenticates the inbound stream;
// there is no naozhi-side HMAC verification path to bypass and no empty-
// secret fallback to misconfigure. RegisterRoutes is a documented no-op
// (slack.go:103). This test pins that no-op so a future refactor that
// adds an HTTP webhook surface without an HMAC signing-secret check
// fails compile-time / test-time, surfacing the security review need
// before the bypass ships.
//
// If a future change DOES need an HTTP webhook (e.g. for slash commands
// or interactive components), this test must be updated AND the new
// surface must validate X-Slack-Signature with a constant-time HMAC
// compare against a non-empty signing secret — empty-secret config must
// hard-reject (parity with feishu's EncryptKey/VerificationToken gates,
// see internal/platform/feishu/transport_hook.go:85-95).
func TestNoSigningSecretSurface(t *testing.T) {
	t.Parallel()

	// Config has only BotToken + AppToken + MaxReplyLen. There is no
	// SigningSecret field: the empty-secret path the issue worried about
	// cannot exist because no field exists to leave empty.
	cfg := Config{BotToken: "xoxb-test", AppToken: "xapp-test"}
	s := New(cfg)

	// RegisterRoutes must remain a no-op so the HTTP mux acquires no
	// inbound handler from the Slack adapter. If a future change wires
	// in /webhook/slack, the mux below will gain a route and the test
	// fails with a clear migration prompt.
	mux := http.NewServeMux()
	noop := func(_ context.Context, _ platform.IncomingMessage) {}
	s.RegisterRoutes(mux, noop)

	// Probe the mux for any registered handler on common Slack webhook
	// paths. http.ServeMux returns the default NotFoundHandler when no
	// pattern matches; we assert that explicitly.
	for _, path := range []string{
		"/webhook/slack",
		"/slack/events",
		"/slack/interactive",
		"/slack/commands",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		_, pattern := mux.Handler(req)
		if pattern != "" {
			t.Errorf("Slack adapter registered HTTP route %q (pattern=%q) — adapter must remain Socket Mode only "+
				"OR add a constant-time HMAC signing-secret check (issue #879 / R244-SEC-P1-4); "+
				"empty SigningSecret must hard-reject like feishu transport_hook.go:85-95",
				path, pattern)
		}
	}
}
