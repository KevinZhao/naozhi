package feishu

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
)

// Voice-pipeline user-facing notices, centralised as the future i18n seam.
const (
	msgVoiceDownloadFailed   = "[语音消息下载失败，请重试]"
	msgVoiceTranscribeFailed = "[语音消息转写失败，请发送文字消息]"
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

	// f.dispatch bounds handler goroutines under message bursts; only one
	// transport is active per adapter, so sharing the pool is harmless.
	eventHandler := dispatcher.NewEventDispatcher(
		f.cfg.VerificationToken, f.cfg.EncryptKey,
	).OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		pe, ok := f.parseSDKEvent(event)
		if !ok {
			return nil
		}

		// TryGo does wg.Add(1) on this goroutine before `go`, so a concurrent
		// Stop()/Wait() cannot observe counter=0 mid-dispatch.
		switch pe.MediaType {
		case "image":
			f.dispatch.TryGo("feishu ws image", func() {
				msg := pe.Msg
				data, mime, err := f.DownloadImage(ctx, pe.MessageID, pe.MediaKey)
				if err != nil {
					// image_key is sender-controlled; sanitize before slog.
					slog.Error("feishu ws download image failed", "err", err,
						"key", osutil.SanitizeForLog(pe.MediaKey, 128))
					return
				}
				msg.Images = []platform.Image{{Data: data, MimeType: mime}}
				handler(ctx, msg)
			})

		case "audio":
			f.dispatch.TryGo("feishu ws audio", func() {
				msg := pe.Msg
				f.handleAudio(ctx, handler, msg, pe.MessageID, pe.MediaKey)
			})

		default:
			f.dispatch.TryGo("feishu ws text", func() { handler(ctx, pe.Msg) })
		}
		return nil
	}).OnP2CardActionTrigger(func(cardCtx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		// The SDK requires a non-nil response; an empty Toast keeps the client silent.
		if event == nil || event.Event == nil || event.Event.Action == nil {
			return &callback.CardActionTriggerResponse{}, nil
		}
		// Round-trip Action.Value (map[string]any) into the typed payload.
		raw, err := json.Marshal(event.Event.Action.Value)
		if err != nil {
			slog.Warn("feishu ws card_action: marshal value failed", "err", err)
			return &callback.CardActionTriggerResponse{}, nil
		}
		var val cardActionPayload
		if err := json.Unmarshal(raw, &val); err != nil {
			slog.Warn("feishu ws card_action: decode value failed", "err", err)
			return &callback.CardActionTriggerResponse{}, nil
		}
		var chatID, messageID string
		if event.Event.Context != nil {
			chatID = event.Event.Context.OpenChatID
			messageID = event.Event.Context.OpenMessageID
		}
		operatorID := ""
		if event.Event.Operator != nil {
			operatorID = event.Event.Operator.OpenID
		}
		// The WS callback carries no chat_type; use the value embedded in the
		// button, defaulting to "direct" (p2p chats also use "oc_" ids, so a
		// prefix heuristic would mis-route 1:1 answers).
		chatType := normalizeCardChatType(val.ChatType)
		if chatType == "" {
			chatType = "direct"
		}
		f.dispatchCardActionTracked(cardCtx, val, chatID, messageID, chatType, operatorID, handler)
		return &callback.CardActionTriggerResponse{}, nil
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

// dispatchCardActionTracked runs dispatchCardAction under f.dispatch.TryRun —
// tracked and capped like the message branches, but synchronous because the
// larkws SDK acks the frame only after the callback returns. On a full pool
// the click is dropped best-effort rather than blocking the SDK read loop (#1964).
func (f *Feishu) dispatchCardActionTracked(
	ctx context.Context,
	val cardActionPayload,
	chatID, messageID, chatType, operatorID string,
	handler platform.MessageHandler,
) {
	f.dispatch.TryRun("feishu ws card_action", func() {
		f.dispatchCardAction(ctx, val, chatID, messageID, chatType, operatorID, handler)
	})
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
		// file_key is sender-controlled; sanitize before slog.
		slog.Error("feishu download audio failed", "err", err,
			"key", osutil.SanitizeForLog(fileKey, 128))
		f.replyError(ctx, msg.ChatID, msgVoiceDownloadFailed)
		return
	}

	transcribeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	text, err := f.transcriber.Transcribe(transcribeCtx, data, mime)
	if err != nil {
		slog.Error("feishu transcribe failed", "err", err, "mime", mime, "size", len(data))
		f.replyError(ctx, msg.ChatID, msgVoiceTranscribeFailed)
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
func (f *Feishu) parseSDKEvent(event *larkim.P2MessageReceiveV1) (parsedEvent, bool) {
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
	// Symmetric with transport_hook's cap: bound the dedup map key size.
	if len(eventID) > maxEventIDLen {
		slog.Warn("feishu ws: event_id too long, skipping dedup for this delivery",
			"len", len(eventID))
		eventID = ""
	}

	messageID := ""
	if msg.MessageId != nil {
		messageID = *msg.MessageId
	}

	// nil-safe on every dereference: the SDK returns pointers throughout.
	hasMention := f.isBotMentioned(len(msg.Mentions), func(i int) string {
		m := msg.Mentions[i]
		if m == nil || m.Id == nil || m.Id.OpenId == nil {
			return ""
		}
		return *m.Id.OpenId
	})

	result := platform.IncomingMessage{
		Platform:  "feishu",
		EventID:   eventID,
		MessageID: messageID,
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
		// Same ingress cap as transport_hook.go so neither transport silently
		// accepts what the other rejects.
		if len(text) > maxIncomingTextBytes {
			slog.Warn("feishu ws: text exceeds limit, dropping",
				"size", len(text))
			return parsedEvent{}, false
		}
		// Strip all @-mention tokens in a single pass.
		if len(msg.Mentions) > 0 {
			pairs := make([]string, 0, len(msg.Mentions)*2)
			for _, m := range msg.Mentions {
				if m.Key != nil {
					pairs = append(pairs, *m.Key, "")
				}
			}
			if len(pairs) > 0 {
				text = strings.NewReplacer(pairs...).Replace(text)
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
