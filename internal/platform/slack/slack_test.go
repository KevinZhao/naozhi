package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var (
	_ platform.RunnablePlatform   = (*Slack)(nil)
	_ platform.Reactor            = (*Slack)(nil)
	_ platform.QuestionCardSender = (*Slack)(nil)
	_ platform.ConnStateReporter  = (*Slack)(nil)
)

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

// TestHandleMessage_Skipped: bot messages, edits and other subtypes, and a
// text that is empty once the bot mention is stripped never reach the handler.
func TestHandleMessage_Skipped(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]*slackevents.MessageEvent{
		"bot message":           {BotID: "B123", Text: "hello"},
		"subtype":               {SubType: "message_changed", Text: "hello"},
		"empty after the strip": {Text: "<@U123>"},
	} {
		s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
		s.botID = "U123"
		called := false
		s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
		s.handleMessage(ev)
		if called {
			t.Errorf("%s: reached the handler, want skipped", name)
		}
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
	if received.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct for an im channel", received.ChatType)
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

// TestMaybeHealBotID_RecoversBotID: with botID unknown, one background
// auth.test fills it in (#1947), and the cooldown keeps a burst of messages
// from sending more than one.
func TestMaybeHealBotID_RecoversBotID(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"U999","team":"T"}`))
	}))
	t.Cleanup(srv.Close)
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.api = slack.New("xoxb-test", slack.OptionHTTPClient(slackHTTPClient), slack.OptionAPIURL(srv.URL+"/"))
	for i := 0; i < 3; i++ {
		s.maybeHealBotID()
	}
	s.dispatch.Wait()
	if got := s.botIDOrEmpty(); got != "U999" {
		t.Errorf("botID = %q after the heal, want U999", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("auth.test called %d times, want 1 (cooldown)", n)
	}
}
