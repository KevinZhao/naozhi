package slack

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/slack-go/slack/slackevents"
)

var _ platform.RunnablePlatform = (*Slack)(nil)

func TestNew_Defaults(t *testing.T) {
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	if s.Name() != "slack" {
		t.Errorf("Name() = %q, want slack", s.Name())
	}
	if s.MaxReplyLength() != 4000 {
		t.Errorf("MaxReplyLength() = %d, want 4000", s.MaxReplyLength())
	}
}

func TestNew_CustomMaxReplyLen(t *testing.T) {
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test", MaxReplyLen: 2000})
	if s.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", s.MaxReplyLength())
	}
}

func TestStartAlreadyStarted(t *testing.T) {
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
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	if err := s.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestEditMessage_InvalidFormat(t *testing.T) {
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	err := s.EditMessage(context.Background(), "no-colon-here", "text")
	if err == nil {
		t.Error("expected error for invalid msgID format")
	}
}

func TestHandleMessage_BotMessage(t *testing.T) {
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	called := false
	s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	s.handleMessage(&slackevents.MessageEvent{BotID: "B123", Text: "hello"})
	if called {
		t.Error("bot messages should be skipped")
	}
}

func TestHandleMessage_SubType(t *testing.T) {
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	called := false
	s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	s.handleMessage(&slackevents.MessageEvent{SubType: "message_changed", Text: "hello"})
	if called {
		t.Error("subtype messages should be skipped")
	}
}

func TestHandleMessage_MentionStrip(t *testing.T) {
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
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	s.botID = "U123"
	called := false
	s.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	s.handleMessage(&slackevents.MessageEvent{Text: "<@U123>"})
	if called {
		t.Error("empty text after mention strip should be skipped")
	}
}
