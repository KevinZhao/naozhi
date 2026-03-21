package feishu

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/naozhi/naozhi/internal/platform"
)

func (f *Feishu) startWebSocket() error {
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan struct{})

	handler := f.handler

	eventHandler := dispatcher.NewEventDispatcher(
		f.cfg.VerificationToken, f.cfg.EncryptKey,
	).OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		msg, ok := parseSDKEvent(event)
		if !ok {
			return nil
		}
		go handler(context.Background(), msg)
		return nil
	})

	cli := larkws.NewClient(f.cfg.AppID, f.cfg.AppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	go func() {
		defer close(f.done)
		slog.Info("feishu websocket starting", "app_id", f.cfg.AppID)
		if err := cli.Start(ctx); err != nil && ctx.Err() == nil {
			slog.Error("feishu websocket error", "err", err)
		}
		slog.Info("feishu websocket stopped")
	}()

	return nil
}

// parseSDKEvent converts a Feishu SDK event to platform.IncomingMessage.
func parseSDKEvent(event *larkim.P2MessageReceiveV1) (platform.IncomingMessage, bool) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return platform.IncomingMessage{}, false
	}

	msg := event.Event.Message
	if msg.MessageType == nil || *msg.MessageType != "text" {
		return platform.IncomingMessage{}, false
	}

	// Extract text content from JSON
	if msg.Content == nil {
		return platform.IncomingMessage{}, false
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*msg.Content), &content); err != nil {
		return platform.IncomingMessage{}, false
	}

	text := content.Text
	hasMention := len(msg.Mentions) > 0
	for _, m := range msg.Mentions {
		if m.Key != nil {
			text = strings.ReplaceAll(text, *m.Key, "")
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return platform.IncomingMessage{}, false
	}

	chatType := "direct"
	if msg.ChatType != nil && *msg.ChatType == "group" {
		chatType = "group"
	}

	userID := ""
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil && event.Event.Sender.SenderId.OpenId != nil {
		userID = *event.Event.Sender.SenderId.OpenId
	}

	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}

	eventID := ""
	if event.EventV2Base != nil && event.EventV2Base.Header != nil {
		eventID = event.EventV2Base.Header.EventID
	}

	return platform.IncomingMessage{
		Platform:  "feishu",
		EventID:   eventID,
		UserID:    userID,
		ChatID:    chatID,
		ChatType:  chatType,
		Text:      text,
		MentionMe: hasMention,
	}, true
}
