package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// TestStart_RejectsEmptyCredentials pins the R244-SEC-P1-4 (#879) guard:
// Start() must hard-reject empty BotToken or AppToken so a misconfigured
// deployment fails loudly at lifecycle entry rather than burning a
// connection attempt against slack.com and surfacing an opaque
// "missing_scope"/"invalid_auth" further down the stack. The empty-
// credential path is the Socket-Mode equivalent of feishu's empty
// signing-secret fallback (transport_hook.go:30-34); both must hard-fail.
func TestStart_RejectsEmptyCredentials(t *testing.T) {
	t.Parallel()
	noop := func(_ context.Context, _ platform.IncomingMessage) {}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty BotToken", Config{BotToken: "", AppToken: "xapp-test"}},
		{"empty AppToken", Config{BotToken: "xoxb-test", AppToken: ""}},
		{"both empty", Config{BotToken: "", AppToken: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(tc.cfg)
			err := s.Start(noop)
			if err == nil {
				t.Fatalf("Start() with %s must reject empty credential", tc.name)
			}
			// Ensure no lifecycle goroutines were started — s.started
			// must remain false so a subsequent Start() (after the
			// operator fixes config) is not blocked by the stale flag.
			s.startMu.Lock()
			started := s.started
			s.startMu.Unlock()
			if started {
				t.Errorf("Start() rejected %s but s.started=true; subsequent valid Start() would be blocked", tc.name)
			}
		})
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

// TestHandleMessage_EventIDIsChannelScoped pins #2015: the global dedup key
// (EventID) must be channel-scoped, not a bare Slack ts. Slack documents that
// two messages in different channels can share a ts, so a bare-ts EventID would
// let the second channel's message dedup against the first and be silently
// dropped. EventID must equal the (channel,ts) composite — the same key
// MessageID already uses.
func TestHandleMessage_EventIDIsChannelScoped(t *testing.T) {
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
		User: "U456", Channel: "C789", ChannelType: "im",
		Text: "hi", TimeStamp: "1234567890.000100",
	})
	<-done
	want := "C789:1234567890.000100"
	if received.EventID != want {
		t.Errorf("EventID = %q, want %q (must be channel-scoped, not bare ts)", received.EventID, want)
	}
	if received.EventID == "1234567890.000100" {
		t.Error("EventID is a bare ts — collides across channels (#2015)")
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

// TestHandleMessage_EmptyBotIDFailsOpenForGroup pins #1947: when AuthTest
// failed at Start so botID is unknown, a group message must be marked
// MentionMe=true (fail-open) rather than MentionMe=false. The Start-time warn
// promises "all channel messages will be processed (no mention filtering)";
// the previous code left MentionMe=false, which the dispatcher's group gate
// (ChatType=="group" && !MentionMe) turned into a silent fail-CLOSED drop of
// every group message until restart.
func TestHandleMessage_EmptyBotIDFailsOpenForGroup(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	// botID intentionally left "" (AuthTest failed). Push the self-heal
	// cooldown into the future so this test never makes a live auth.test call.
	s.botHealAt = time.Now().Add(time.Hour)

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
		Text:        "hello bot",
		TimeStamp:   "1234567890.000300",
	})
	<-done
	if !received.MentionMe {
		t.Fatal("MentionMe must be true when botID is unknown (fail-open) so the group gate does not silently drop the message (#1947)")
	}
	if received.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", received.ChatType)
	}
	if received.Text != "hello bot" {
		t.Errorf("Text = %q, want 'hello bot'", received.Text)
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
