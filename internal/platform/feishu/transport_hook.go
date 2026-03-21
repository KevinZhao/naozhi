package feishu

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/platform"
)

// registerWebhook registers the Feishu webhook HTTP handler.
func (f *Feishu) registerWebhook(mux *http.ServeMux, handler platform.MessageHandler) {
	mux.HandleFunc("POST /webhook/feishu", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		slog.Debug("feishu webhook received", "body_len", len(body), "body", string(body[:min(len(body), 500)]))

		// Parse the outer envelope
		var envelope struct {
			Challenge string `json:"challenge"`
			Token     string `json:"token"`
			Type      string `json:"type"`
			Schema    string `json:"schema"`
			Header    *struct {
				EventID   string `json:"event_id"`
				EventType string `json:"event_type"`
				Token     string `json:"token"`
			} `json:"header"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Challenge verification
		if envelope.Type == "url_verification" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
			return
		}

		// Token verification (v1: top-level token, v2: header.token)
		if f.cfg.VerificationToken != "" {
			token := envelope.Token
			if envelope.Header != nil && envelope.Header.Token != "" {
				token = envelope.Header.Token
			}
			if token != f.cfg.VerificationToken {
				slog.Warn("feishu token mismatch")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Signature verification (v2 events with encrypt_key)
		if f.cfg.EncryptKey != "" {
			timestamp := r.Header.Get("X-Lark-Request-Timestamp")
			nonce := r.Header.Get("X-Lark-Request-Nonce")
			sig := r.Header.Get("X-Lark-Signature")
			if !verifySignature(timestamp, nonce, f.cfg.EncryptKey, body, sig) {
				slog.Warn("feishu signature verification failed")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Return 200 immediately
		w.WriteHeader(http.StatusOK)

		// Only handle message events
		eventType := ""
		if envelope.Header != nil {
			eventType = envelope.Header.EventType
		}
		if eventType != "im.message.receive_v1" {
			return
		}

		// Parse message event
		var event struct {
			Sender struct {
				SenderID struct {
					OpenID string `json:"open_id"`
				} `json:"sender_id"`
			} `json:"sender"`
			Message struct {
				MessageID   string `json:"message_id"`
				ChatID      string `json:"chat_id"`
				ChatType    string `json:"chat_type"`
				Content     string `json:"content"`
				MessageType string `json:"message_type"`
				Mentions    []struct {
					Key  string `json:"key"`
					Name string `json:"name"`
				} `json:"mentions"`
			} `json:"message"`
		}
		if err := json.Unmarshal(envelope.Event, &event); err != nil {
			slog.Error("parse feishu event", "err", err)
			return
		}

		// Only handle text messages
		if event.Message.MessageType != "text" {
			return
		}

		// Extract text content
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
			return
		}

		text := content.Text
		// Remove @mention prefix
		for _, m := range event.Message.Mentions {
			text = strings.ReplaceAll(text, m.Key, "")
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}

		// Build incoming message
		eventID := ""
		if envelope.Header != nil {
			eventID = envelope.Header.EventID
		}

		chatType := "direct"
		if event.Message.ChatType == "group" {
			chatType = "group"
		}

		msg := platform.IncomingMessage{
			Platform:  "feishu",
			EventID:   eventID,
			UserID:    event.Sender.SenderID.OpenID,
			ChatID:    event.Message.ChatID,
			ChatType:  chatType,
			Text:      text,
			MentionMe: len(event.Message.Mentions) > 0,
		}

		// Async processing (use background context, not request context)
		go handler(context.Background(), msg)
	})
}
