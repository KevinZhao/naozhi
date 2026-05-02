package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/naozhi/naozhi/internal/platform"
)

var _ platform.RunnablePlatform = (*Discord)(nil)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if d.Name() != "discord" {
		t.Errorf("Name() = %q, want discord", d.Name())
	}
	if d.MaxReplyLength() != 2000 {
		t.Errorf("MaxReplyLength() = %d, want 2000", d.MaxReplyLength())
	}
}

func TestNew_CustomMaxReplyLen(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token", MaxReplyLen: 1500})
	if d.MaxReplyLength() != 1500 {
		t.Errorf("MaxReplyLength() = %d, want 1500", d.MaxReplyLength())
	}
}

func TestStartAlreadyStarted(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.startMu.Lock()
	d.started = true
	d.startMu.Unlock()
	noop := func(_ context.Context, _ platform.IncomingMessage) {}
	err := d.Start(noop)
	if err == nil {
		t.Error("expected error for double Start()")
	}
}

func TestStopNoop(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if err := d.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestEditMessage_InvalidFormat(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	err := d.EditMessage(context.Background(), "no-colon-here", "text")
	if err == nil {
		t.Error("expected error for invalid msgID format")
	}
}

func TestOnMessageCreate_NilAuthor(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	// Should not panic
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{Author: nil, Content: "hello"},
	})
	if called {
		t.Error("nil author messages should be skipped")
	}
}

func TestOnMessageCreate_BotMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author:  &discordgo.User{ID: "other_bot", Bot: true},
			Content: "hello",
		},
	})
	if called {
		t.Error("bot messages should be skipped")
	}
}

func TestOnMessageCreate_OwnMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author:  &discordgo.User{ID: "bot123"},
			Content: "hello",
		},
	})
	if called {
		t.Error("own messages should be skipped")
	}
}

func TestOnMessageCreate_GroupMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg1",
			Author:    &discordgo.User{ID: "user1"},
			Content:   "hello",
			ChannelID: "ch1",
			GuildID:   "guild1",
		},
	})
	<-done
	if received.ChatType != "group" {
		t.Errorf("ChatType = %q, want group", received.ChatType)
	}
	if received.Platform != "discord" {
		t.Errorf("Platform = %q, want discord", received.Platform)
	}
}

func TestOnMessageCreate_DirectMessage(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg2",
			Author:    &discordgo.User{ID: "user2"},
			Content:   "hello dm",
			ChannelID: "dm_ch",
			GuildID:   "", // empty = DM
		},
	})
	<-done
	if received.ChatType != "direct" {
		t.Errorf("ChatType = %q, want direct", received.ChatType)
	}
}

func TestOnMessageCreate_MentionStrip(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	var received platform.IncomingMessage
	done := make(chan struct{})
	d.handler = func(_ context.Context, msg platform.IncomingMessage) {
		received = msg
		close(done)
	}
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg3",
			Author:    &discordgo.User{ID: "user3"},
			Content:   "<@bot123> do something",
			ChannelID: "ch3",
			GuildID:   "g1",
			Mentions:  []*discordgo.User{{ID: "bot123"}},
		},
	})
	<-done
	if received.Text != "do something" {
		t.Errorf("Text = %q, want 'do something'", received.Text)
	}
	if !received.MentionMe {
		t.Error("MentionMe should be true")
	}
}

func TestOnMessageCreate_EmptyAfterMentionStrip(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.botID = "bot123"
	called := false
	d.handler = func(_ context.Context, _ platform.IncomingMessage) { called = true }
	d.onMessageCreate(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Author:   &discordgo.User{ID: "user1"},
			Content:  "<@bot123>",
			Mentions: []*discordgo.User{{ID: "bot123"}},
		},
	})
	if called {
		t.Error("empty text after mention strip should be skipped")
	}
}
