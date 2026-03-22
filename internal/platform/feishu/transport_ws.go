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
		msg, messageID, imageKey, ok := parseSDKEvent(event)
		if !ok {
			return nil
		}
		if imageKey != "" {
			go func() {
				data, mime, err := f.DownloadImage(ctx, messageID, imageKey)
				if err != nil {
					slog.Error("feishu ws download image failed", "err", err, "key", imageKey)
					return
				}
				msg.Images = []platform.Image{{Data: data, MimeType: mime}}
				handler(ctx, msg)
			}()
		} else {
			go handler(ctx, msg)
		}
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
// Returns the message, message_id, an image_key (non-empty for image messages), and whether parsing succeeded.
func parseSDKEvent(event *larkim.P2MessageReceiveV1) (platform.IncomingMessage, string, string, bool) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return platform.IncomingMessage{}, "", "", false
	}

	msg := event.Event.Message
	if msg.MessageType == nil {
		return platform.IncomingMessage{}, "", "", false
	}

	msgType := *msg.MessageType
	if msgType != "text" && msgType != "image" {
		return platform.IncomingMessage{}, "", "", false
	}

	if msg.Content == nil {
		return platform.IncomingMessage{}, "", "", false
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

	messageID := ""
	if msg.MessageId != nil {
		messageID = *msg.MessageId
	}

	hasMention := len(msg.Mentions) > 0

	result := platform.IncomingMessage{
		Platform:  "feishu",
		EventID:   eventID,
		UserID:    userID,
		ChatID:    chatID,
		ChatType:  chatType,
		MentionMe: hasMention,
	}

	switch msgType {
	case "text":
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &content); err != nil {
			return platform.IncomingMessage{}, "", "", false
		}
		text := content.Text
		for _, m := range msg.Mentions {
			if m.Key != nil {
				text = strings.ReplaceAll(text, *m.Key, "")
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return platform.IncomingMessage{}, "", "", false
		}
		result.Text = text
		return result, "", "", true

	case "image":
		var content struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &content); err != nil || content.ImageKey == "" {
			return platform.IncomingMessage{}, "", "", false
		}
		return result, messageID, content.ImageKey, true

	default:
		return platform.IncomingMessage{}, "", "", false
	}
}
