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

// Voice-pipeline user-facing notices. Centralised as named constants
// (R247-ARCH-19, #631) so the platform layer no longer scatters inline
// user-facing literals across the audio handler — a single source of
// truth here is the seam the future i18n catalog (internal/i18n) hooks
// into without re-grepping the adapter body.
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

	// Limit concurrent message handlers to avoid unbounded goroutine growth
	// when Feishu delivers bursts of messages (e.g., group chat floods).
	msgSem := make(chan struct{}, 20)

	eventHandler := dispatcher.NewEventDispatcher(
		f.cfg.VerificationToken, f.cfg.EncryptKey,
	).OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
		pe, ok := f.parseSDKEvent(event)
		if !ok {
			return nil
		}

		// R184-CONCUR-H1: wg.Add(1) MUST precede the msgSem select, so a
		// concurrent Stop()/wg.Wait() cannot observe counter=0 between the
		// goroutine being dispatched and wg.Add running. The drop branch
		// must balance with wg.Done(). Mirrors the wshub.go invariant
		// where clientWG.Add(2) precedes register().
		switch pe.MediaType {
		case "image":
			f.wg.Add(1)
			select {
			case msgSem <- struct{}{}:
			default:
				f.wg.Done()
				slog.Warn("feishu ws: handler semaphore full, dropping image message")
				return nil
			}
			go func() {
				defer f.wg.Done()
				defer func() { <-msgSem }()
				defer platform.RecoverHandler("feishu ws image")
				msg := pe.Msg
				data, mime, err := f.DownloadImage(ctx, pe.MessageID, pe.MediaKey)
				if err != nil {
					// R190-SEC-M3: pe.MediaKey is derived from user-crafted
					// Feishu message content (image_key). Sanitize before slog
					// so C1/bidi/LS/PS can't fragment structured-log fields.
					slog.Error("feishu ws download image failed", "err", err,
						"key", osutil.SanitizeForLog(pe.MediaKey, 128))
					return
				}
				msg.Images = []platform.Image{{Data: data, MimeType: mime}}
				handler(ctx, msg)
			}()

		case "audio":
			f.wg.Add(1)
			select {
			case msgSem <- struct{}{}:
			default:
				f.wg.Done()
				slog.Warn("feishu ws: handler semaphore full, dropping audio message")
				return nil
			}
			go func() {
				defer f.wg.Done()
				defer func() { <-msgSem }()
				defer platform.RecoverHandler("feishu ws audio")
				msg := pe.Msg
				f.handleAudio(ctx, handler, msg, pe.MessageID, pe.MediaKey)
			}()

		default:
			f.wg.Add(1)
			select {
			case msgSem <- struct{}{}:
			default:
				f.wg.Done()
				slog.Warn("feishu ws: handler semaphore full, dropping message")
				return nil
			}
			go func() {
				defer f.wg.Done()
				defer func() { <-msgSem }()
				defer platform.RecoverHandler("feishu ws text")
				handler(ctx, pe.Msg)
			}()
		}
		return nil
	}).OnP2CardActionTrigger(func(cardCtx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		// Card button click — decode the `value` payload we emitted in
		// SendQuestionCard and dispatch as a synthesised user message.
		// We must return non-nil because the SDK expects a response; an
		// empty Toast keeps the Feishu client silent (no popup spam).
		if event == nil || event.Event == nil || event.Event.Action == nil {
			return &callback.CardActionTriggerResponse{}, nil
		}
		// Decode Action.Value (map[string]any) into our typed payload so the
		// downstream dispatchCardAction path is uniform across WS + webhook.
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
		// Card actions don't carry a chat_type; we infer from chat_id prefix:
		// "oc_" is a group chat (open_chat_id), "ou_" would be direct (open_id
		// used as chat target in 1:1). Feishu's card actions originate from a
		// message in a chat, so oc_ indicates group; anything else we call
		// direct. Defensive default: group chats get authorization via
		// dispatch's own mention/owner rules.
		chatType := "direct"
		if strings.HasPrefix(chatID, "oc_") {
			chatType = "group"
		}
		f.dispatchCardAction(cardCtx, val, chatID, messageID, chatType, operatorID, handler)
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

// handleAudio downloads and transcribes audio, then calls handler with the text.
// Errors are replied directly to the user, not sent through Claude.
func (f *Feishu) handleAudio(ctx context.Context, handler platform.MessageHandler, msg platform.IncomingMessage, messageID, fileKey string) {
	if f.transcriber == nil {
		slog.Info("feishu audio ignored, transcriber not configured", "user", msg.UserID)
		return
	}

	data, mime, err := f.DownloadAudio(ctx, messageID, fileKey)
	if err != nil {
		// R190-SEC-M3: fileKey originates from user-crafted Feishu message
		// content (file_key in audio messages). Sanitize before slog.
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
//
// Method receiver (rather than package function) so we can call
// f.isBotMentioned against the bot's cached open_id for precise group-chat
// mention detection.
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
	// R186-SEC-L: symmetric with transport_hook's cap. WS path is signed by
	// Feishu's SDK, but the dedup map still grows one key per delivered event;
	// bounding the key size guards against an unexpected wire-format bump.
	if len(eventID) > maxEventIDLen {
		slog.Warn("feishu ws: event_id too long, skipping dedup for this delivery",
			"len", len(eventID))
		eventID = ""
	}

	messageID := ""
	if msg.MessageId != nil {
		messageID = *msg.MessageId
	}

	// Precise bot-mention detection: match each mention's id.open_id against
	// the bot's own open_id cached by fetchBotInfo(). Degrades to "any mention"
	// when the bot open_id is unknown (see isBotMentioned). nil-safe on every
	// pointer dereference because the SDK returns pointers throughout.
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
		// Mirror the webhook-path guard in transport_hook.go: reject oversized
		// text bodies at the ingress boundary so multi-KB payloads never reach
		// slog attrs, dispatch queue, or CLI stdin. Feishu's upstream limit is
		// ~4 KiB (~1333 CJK chars); 8 KiB is 2× that official ceiling.
		// Keeping WS and webhook paths symmetric prevents a regression where
		// one transport silently accepts what the other rejects. R184-SEC-H1a.
		// maxIncomingTextBytes is shared with transport_hook.go.
		if len(text) > maxIncomingTextBytes {
			slog.Warn("feishu ws: text exceeds limit, dropping",
				"size", len(text))
			return parsedEvent{}, false
		}
		// Strip all @-mention tokens in a single pass, mirroring the
		// webhook path in transport_hook.go. Previously each ReplaceAll
		// allocated a fresh string and copied the whole text; a group
		// message with multiple @ users did that N times. WS is the
		// default transport, so it carries the bulk of inbound traffic.
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
