package feishu

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// Config holds Feishu app credentials.
type Config struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLen       int    `yaml:"max_reply_length"`
}

// Feishu implements the Platform interface for Feishu (Lark).
type Feishu struct {
	cfg         Config
	accessToken string
	tokenExpiry time.Time
	tokenMu     sync.Mutex
}

// New creates a Feishu platform adapter.
func New(cfg Config) *Feishu {
	if cfg.MaxReplyLen <= 0 {
		cfg.MaxReplyLen = 4000
	}
	return &Feishu{cfg: cfg}
}

func (f *Feishu) Name() string { return "feishu" }

func (f *Feishu) MaxReplyLength() int { return f.cfg.MaxReplyLen }

// RegisterRoutes registers the Feishu webhook handler.
func (f *Feishu) RegisterRoutes(mux *http.ServeMux, handler platform.MessageHandler) {
	mux.HandleFunc("POST /webhook/feishu", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		slog.Info("feishu webhook received", "body_len", len(body), "body", string(body[:min(len(body), 500)]))

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

		// Token verification (v1 events)
		if envelope.Token != "" && f.cfg.VerificationToken != "" {
			if envelope.Token != f.cfg.VerificationToken {
				slog.Warn("feishu token mismatch")
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

// Reply sends a text message to a Feishu chat.
func (f *Feishu) Reply(ctx context.Context, msg platform.OutgoingMessage) (string, error) {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}

	content, _ := json.Marshal(map[string]string{"text": msg.Text})
	reqBody, _ := json.Marshal(map[string]interface{}{
		"receive_id": msg.ChatID,
		"msg_type":   "text",
		"content":    string(content),
	})

	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int `json:"code"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("feishu api error: code=%d msg=%s", result.Code, result.Msg)
	}

	return result.Data.MessageID, nil
}

// EditMessage updates an existing Feishu message.
func (f *Feishu) EditMessage(ctx context.Context, msgID string, text string) error {
	token, err := f.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	content, _ := json.Marshal(map[string]string{"text": text})
	reqBody, _ := json.Marshal(map[string]interface{}{
		"msg_type": "text",
		"content":  string(content),
	})

	req, _ := http.NewRequestWithContext(ctx, "PUT",
		"https://open.feishu.cn/open-apis/im/v1/messages/"+msgID,
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("edit message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.Code != 0 {
		return fmt.Errorf("feishu edit error: code=%d msg=%s", result.Code, result.Msg)
	}
	return nil
}

// getAccessToken returns a valid tenant access token, refreshing if needed.
func (f *Feishu) getAccessToken(ctx context.Context) (string, error) {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
		return f.accessToken, nil
	}

	reqBody, _ := json.Marshal(map[string]string{
		"app_id":     f.cfg.AppID,
		"app_secret": f.cfg.AppSecret,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Code != 0 {
		return "", fmt.Errorf("get token error: code=%d", result.Code)
	}

	f.accessToken = result.TenantAccessToken
	f.tokenExpiry = time.Now().Add(time.Duration(result.Expire-60) * time.Second) // refresh 60s early
	return f.accessToken, nil
}

// verifySignature verifies the request signature (for encrypt_key mode).
func verifySignature(timestamp, nonce, encryptKey string, body []byte, signature string) bool {
	if encryptKey == "" {
		return true
	}
	content := timestamp + nonce + encryptKey + string(body)
	h := hmac.New(sha256.New, []byte(""))
	h.Write([]byte(content))
	expected := fmt.Sprintf("%x", h.Sum(nil))
	return expected == signature
}
