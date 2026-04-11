package feishu

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/naozhi/naozhi/internal/platform"
)

// parsedEvent holds the result of parsing a Feishu SDK event.
type parsedEvent struct {
	Msg       platform.IncomingMessage
	MessageID string
	MediaType string // "" | "image" | "audio"
	MediaKey  string // imageKey or fileKey
}

func (f *Feishu) startWebSocket() error {
	ctx, cancel := context.WithCancel(context.Background())

	f.startMu.Lock()
	f.cancel = cancel
	f.done = make(chan struct{})
	f.startMu.Unlock()

	handler := f.handler

	// Limit concurrent message handlers to avoid unbounded goroutine growth
	// when Feishu delivers bursts of messages (e.g., group chat floods).
	msgSem := make(chan struct{}, 20)

	eventHandler := dispatcher.NewEventDispatcher(
		f.cfg.VerificationToken, f.cfg.EncryptKey,
	).OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		pe, ok := parseSDKEvent(event)
		if !ok {
			return nil
		}

		switch pe.MediaType {
		case "image":
			select {
			case msgSem <- struct{}{}:
			default:
				slog.Warn("feishu ws: handler semaphore full, dropping image message")
				return nil
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-msgSem }()
				defer platform.RecoverHandler("feishu ws image")
				msg := pe.Msg
				data, mime, err := f.DownloadImage(ctx, pe.MessageID, pe.MediaKey)
				if err != nil {
					slog.Error("feishu ws download image failed", "err", err, "key", pe.MediaKey)
					return
				}
				msg.Images = []platform.Image{{Data: data, MimeType: mime}}
				handler(ctx, msg)
			}()

		case "audio":
			select {
			case msgSem <- struct{}{}:
			default:
				slog.Warn("feishu ws: handler semaphore full, dropping audio message")
				return nil
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-msgSem }()
				defer platform.RecoverHandler("feishu ws audio")
				msg := pe.Msg
				f.handleAudio(ctx, handler, msg, pe.MessageID, pe.MediaKey)
			}()

		default:
			select {
			case msgSem <- struct{}{}:
			default:
				slog.Warn("feishu ws: handler semaphore full, dropping message")
				return nil
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				defer func() { <-msgSem }()
				defer platform.RecoverHandler("feishu ws text")
				handler(ctx, pe.Msg)
			}()
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

// handleAudio downloads and transcribes audio, then calls handler with the text.
// Errors are replied directly to the user, not sent through Claude.
func (f *Feishu) handleAudio(ctx context.Context, handler platform.MessageHandler, msg platform.IncomingMessage, messageID, fileKey string) {
	if f.transcriber == nil {
		slog.Info("feishu audio ignored, transcriber not configured", "user", msg.UserID)
		return
	}

	data, mime, err := f.DownloadAudio(ctx, messageID, fileKey)
	if err != nil {
		slog.Error("feishu download audio failed", "err", err, "key", fileKey)
		f.replyError(ctx, msg.ChatID, "[语音消息下载失败，请重试]")
		return
	}

	transcribeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	text, err := f.transcriber.Transcribe(transcribeCtx, data, mime)
	if err != nil {
		slog.Error("feishu transcribe failed", "err", err, "mime", mime, "size", len(data))
		f.replyError(ctx, msg.ChatID, "[语音消息转写失败，请发送文字消息]")
		return
	}

	if text == "" {
		slog.Debug("feishu transcribe returned empty text", "user", msg.UserID, "size", len(data))
		return
	}

	msg.Text = text
	handler(ctx, msg)
}

// parseSDKEvent converts a Feishu SDK event to a parsedEvent.
func parseSDKEvent(event *larkim.P2MessageReceiveV1) (parsedEvent, bool) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return parsedEvent{}, false
	}

	msg := event.Event.Message
	if msg.MessageType == nil {
		return parsedEvent{}, false
	}

	msgType := *msg.MessageType
	if msgType != "text" && msgType != "image" && msgType != "audio" {
		return parsedEvent{}, false
	}

	if msg.Content == nil {
		return parsedEvent{}, false
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
			return parsedEvent{}, false
		}
		text := content.Text
		for _, m := range msg.Mentions {
			if m.Key != nil {
				text = strings.ReplaceAll(text, *m.Key, "")
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return parsedEvent{}, false
		}
		result.Text = text
		return parsedEvent{Msg: result}, true

	case "image":
		var content struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &content); err != nil || content.ImageKey == "" {
			return parsedEvent{}, false
		}
		return parsedEvent{Msg: result, MessageID: messageID, MediaType: "image", MediaKey: content.ImageKey}, true

	case "audio":
		var content struct {
			FileKey string `json:"file_key"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &content); err != nil || content.FileKey == "" {
			return parsedEvent{}, false
		}
		return parsedEvent{Msg: result, MessageID: messageID, MediaType: "audio", MediaKey: content.FileKey}, true

	default:
		return parsedEvent{}, false
	}
}
